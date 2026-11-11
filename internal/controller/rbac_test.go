// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

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
	mgr, err := ctrl.NewManager(asController, ctrl.Options{
		Scheme:                  controller.Scheme(),
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          true,
		LeaderElectionID:        "controlplane.lodia.io",
		LeaderElectionNamespace: ns,
	})
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
	allowed := func(namespace, group, resource, verb string) bool {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: asController.Impersonate.UserName,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Group: group, Resource: resource, Verb: verb,
			},
		}}
		if err := k8s.Create(t.Context(), review); err != nil {
			t.Fatal(err)
		}
		return review.Status.Allowed
	}
	for _, c := range []struct{ group, resource, verb string }{
		{coordinationv1.GroupName, "leases", "create"},
		{coordinationv1.GroupName, "leases", "get"},
		{coordinationv1.GroupName, "leases", "update"},
		{corev1.GroupName, "events", "create"},
		{corev1.GroupName, "events", "patch"},
	} {
		eventually(t, func() error {
			if !allowed(ns, c.group, c.resource, c.verb) {
				return fmt.Errorf("%s %s is not allowed in %s", c.verb, c.resource, ns)
			}
			return nil
		})
		for _, other := range []string{metav1.NamespaceSystem, corev1.NamespaceNodeLease} {
			if allowed(other, c.group, c.resource, c.verb) {
				t.Errorf("%s %s is allowed in %s", c.verb, c.resource, other)
			}
		}
	}
}
