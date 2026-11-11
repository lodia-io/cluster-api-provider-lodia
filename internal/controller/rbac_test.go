// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

func boundToTheLeaderElectionRole(t *testing.T, ns string) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "leader_election_role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{}
	if err := yaml.UnmarshalStrict(manifest, role); err != nil {
		t.Fatal(err)
	}
	role.Namespace = ns
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: asController.Impersonate.UserName}},
	}
	for _, obj := range []client.Object{role, binding} {
		if err := k8s.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheRoleCoversLeaderElectionAndTheMetricsFilter(t *testing.T) {
	ns := namespace(t)
	boundToTheLeaderElectionRole(t, ns)
	options, err := controller.ManagerOptions("0", "0", true)
	if err != nil {
		t.Fatal(err)
	}
	options.LeaderElectionNamespace = ns
	mgr, err := ctrl.NewManager(asController, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	eventually(t, func() error {
		lease := &coordinationv1.Lease{}
		if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "controlplane.lodia.io"}, lease); err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return errors.New("no holder")
		}
		events := &corev1.EventList{}
		if err := k8s.List(t.Context(), events, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Reason == "LeaderElection" {
				return nil
			}
		}
		return errors.New("no LeaderElection event")
	})

	for _, attributes := range []authorizationv1.ResourceAttributes{
		{Group: authenticationv1.GroupName, Resource: "tokenreviews", Verb: "create"},
		{Group: authorizationv1.GroupName, Resource: "subjectaccessreviews", Verb: "create"},
	} {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: asController.Impersonate.UserName, ResourceAttributes: &attributes,
		}}
		if err := k8s.Create(t.Context(), review); err != nil {
			t.Fatal(err)
		}
		if !review.Status.Allowed {
			t.Errorf("%s %s is not allowed: %s", attributes.Verb, attributes.Resource, review.Status.Reason)
		}
	}
}

func TestLeasesAndEventsAreGrantedOnlyInTheLeaderElectionNamespace(t *testing.T) {
	ns := namespace(t)
	boundToTheLeaderElectionRole(t, ns)
	allowed := func(namespace, group, resource, name, verb string) bool {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: asController.Impersonate.UserName,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Group: group, Resource: resource, Name: name, Verb: verb,
			},
		}}
		if err := k8s.Create(t.Context(), review); err != nil {
			t.Fatal(err)
		}
		return review.Status.Allowed
	}
	for _, c := range []struct{ group, resource, name, verb string }{
		{coordinationv1.GroupName, "leases", "", "create"},
		{coordinationv1.GroupName, "leases", "controlplane.lodia.io", "get"},
		{coordinationv1.GroupName, "leases", "controlplane.lodia.io", "update"},
		{corev1.GroupName, "events", "", "create"},
		{corev1.GroupName, "events", "", "patch"},
	} {
		eventually(t, func() error {
			if !allowed(ns, c.group, c.resource, c.name, c.verb) {
				return fmt.Errorf("%s %s is not allowed in %s", c.verb, c.resource, ns)
			}
			return nil
		})
		for _, other := range []string{metav1.NamespaceSystem, corev1.NamespaceNodeLease} {
			if allowed(other, c.group, c.resource, c.name, c.verb) {
				t.Errorf("%s %s is allowed in %s", c.verb, c.resource, other)
			}
		}
	}
	for _, c := range []struct {
		group, resource, name string
		allow                 []string
	}{
		{coordinationv1.GroupName, "leases", "", []string{"create"}},
		{coordinationv1.GroupName, "leases", "controlplane.lodia.io", []string{"create", "get", "update"}},
		{coordinationv1.GroupName, "leases", "controller-leader-election-capi", []string{"create"}},
		{corev1.GroupName, "events", "", []string{"create", "patch"}},
		{corev1.GroupName, "events", "an-event", []string{"create", "patch"}},
	} {
		for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
			if got, want := allowed(ns, c.group, c.resource, c.name, verb), slices.Contains(c.allow, verb); got != want {
				t.Errorf("%s %s %q in %s: allowed %v, want %v", verb, c.resource, c.name, ns, got, want)
			}
		}
	}
}

func TestTheManagerIsLeaderElectedSecuredAndBuiltOnTheCacheOptions(t *testing.T) {
	for _, flags := range []struct {
		metrics, probes string
		leaderElect     bool
	}{
		{":9443", ":9081", false},
		{"127.0.0.1:9444", "127.0.0.1:9082", true},
	} {
		options, err := controller.ManagerOptions(flags.metrics, flags.probes, flags.leaderElect)
		if err != nil {
			t.Fatal(err)
		}
		if options.LeaderElection != flags.leaderElect || options.LeaderElectionID != "controlplane.lodia.io" {
			t.Errorf("leader election %v with ID %q, want %v", options.LeaderElection, options.LeaderElectionID, flags.leaderElect)
		}
		if m := options.Metrics; m.BindAddress != flags.metrics || !m.SecureServing || m.FilterProvider == nil {
			t.Errorf("metrics %+v: want them on %q over HTTPS behind a filter", m, flags.metrics)
		}
		if options.HealthProbeBindAddress != flags.probes {
			t.Errorf("probes on %q, want %q", options.HealthProbeBindAddress, flags.probes)
		}
	}

	options, err := controller.ManagerOptions(":9443", ":9081", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []schema.GroupVersionKind{
		v1alpha1.GroupVersion.WithKind("LodiaControlPlane"),
		lodiav1alpha1.GroupVersion.WithKind("ControlPlane"),
		clusterv1.GroupVersion.WithKind("Cluster"),
	} {
		if options.Scheme == nil || !options.Scheme.Recognizes(kind) {
			t.Errorf("the scheme does not recognize %s", kind)
		}
	}
	if period := options.Cache.SyncPeriod; period == nil || *period != 10*time.Minute {
		t.Errorf("sync period %v", period)
	}
	if len(options.Cache.DefaultNamespaces) != 0 {
		t.Errorf("the cache is limited to the namespaces %v", options.Cache.DefaultNamespaces)
	}
	selectors := 0
	for object, byObject := range options.Cache.ByObject {
		if _, ok := object.(*corev1.Secret); ok && byObject.Label != nil && byObject.Label.String() == lodiav1alpha1.ControlPlaneLabel {
			selectors++
		}
	}
	if selectors != 1 {
		t.Errorf("%d Secret selectors on Lodia's label, want 1", selectors)
	}
}
