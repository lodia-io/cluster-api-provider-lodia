// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/flags"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

func decodeRBAC(t *testing.T, file string, into runtime.Object) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", file))
	if err != nil {
		t.Fatal(err)
	}
	obj, _, err := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer().Decode(manifest, nil, into)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if obj != into {
		t.Fatalf("%s holds a %T, want a %T", file, obj, into)
	}
}

func leaderElectionRole(t *testing.T) *rbacv1.Role {
	t.Helper()
	role := &rbacv1.Role{}
	decodeRBAC(t, "leader_election_role.yaml", role)
	return role
}

func boundToTheLeaderElectionRole(t *testing.T, ns string) {
	t.Helper()
	role := leaderElectionRole(t)
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
	startManagerWith(t, controller.Config{
		Namespace:     ns,
		Concurrency:   10,
		LeaderElect:   true,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
	}, func(options *ctrl.Options) { options.LeaderElectionNamespace = ns })
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

func TestTheLeaderElectionRoleHoldsNothingBeyondItsLeaseAndEventRules(t *testing.T) {
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{corev1.GroupName}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, ResourceNames: []string{"controlplane.lodia.io"}, Verbs: []string{"get", "update"}},
	}
	if diff := cmp.Diff(want, leaderElectionRole(t).Rules); diff != "" {
		t.Errorf("the Role's rules (-want +got):\n%s", diff)
	}
}

func TestTheLeaderElectionRoleIsBoundOnlyToTheManager(t *testing.T) {
	role := leaderElectionRole(t)
	binding := &rbacv1.RoleBinding{}
	decodeRBAC(t, "leader_election_role_binding.yaml", binding)
	wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}
	if diff := cmp.Diff(wantRef, binding.RoleRef); diff != "" {
		t.Errorf("the binding's roleRef (-want +got):\n%s", diff)
	}
	wantSubjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "manager", Namespace: role.Namespace}}
	if diff := cmp.Diff(wantSubjects, binding.Subjects); diff != "" {
		t.Errorf("the binding's subjects (-want +got):\n%s", diff)
	}
}

func TestTheClusterRoleGrantsNothingOnLeasesOrEvents(t *testing.T) {
	role := &rbacv1.ClusterRole{}
	decodeRBAC(t, "role.yaml", role)
	covers := func(list []string, wildcard, name string) bool {
		return slices.Contains(list, wildcard) || slices.Contains(list, name)
	}
	for i, rule := range role.Rules {
		for _, c := range []struct{ group, resource string }{
			{coordinationv1.GroupName, "leases"},
			{corev1.GroupName, "events"},
			{eventsv1.GroupName, "events"},
		} {
			if covers(rule.APIGroups, rbacv1.APIGroupAll, c.group) && covers(rule.Resources, rbacv1.ResourceAll, c.resource) {
				t.Errorf("the ClusterRole's rule %d grants %v on %s in the group %q", i, rule.Verbs, c.resource, c.group)
			}
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

func TestALeaseUnderAFreeNameCanBeCreatedButAnotherControllersCannotBeReadChangedOrDeleted(t *testing.T) {
	ns := namespace(t)
	boundToTheLeaderElectionRole(t, ns)
	other := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "controller-leader-election-capi"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: new("capi")},
	}
	if err := k8s.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(asController, client.Options{Scheme: controller.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	free := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "a-free-name"}}
	eventually(t, func() error { return c.Create(t.Context(), free) })

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(other), &coordinationv1.Lease{}); !apierrors.IsForbidden(err) {
		t.Errorf("get of another controller's Lease: %v, want forbidden", err)
	}
	other.Spec.HolderIdentity = new("the provider")
	if err := c.Update(t.Context(), other); !apierrors.IsForbidden(err) {
		t.Errorf("update of another controller's Lease: %v, want forbidden", err)
	}
	if err := c.Delete(t.Context(), other); !apierrors.IsForbidden(err) {
		t.Errorf("delete of another controller's Lease: %v, want forbidden", err)
	}
}

func TestTheManagerIsLeaderElectedSecuredAndBuiltOnTheCacheOptions(t *testing.T) {
	for _, c := range []controller.Config{
		{Diagnostics: flags.ManagerOptions{DiagnosticsAddress: ":9443"}, HealthAddr: ":9081"},
		{Diagnostics: flags.ManagerOptions{DiagnosticsAddress: "127.0.0.1:9444"}, HealthAddr: "127.0.0.1:9082", LeaderElect: true},
	} {
		options, err := controller.ManagerOptions(c)
		if err != nil {
			t.Fatal(err)
		}
		if options.LeaderElection != c.LeaderElect || options.LeaderElectionID != "controlplane.lodia.io" {
			t.Errorf("leader election %v with ID %q, want %v", options.LeaderElection, options.LeaderElectionID, c.LeaderElect)
		}
		if options.LeaderElectionNamespace != "" {
			t.Errorf("leader election in the namespace %q, want the Pod's", options.LeaderElectionNamespace)
		}
		if m := options.Metrics; m.BindAddress != c.Diagnostics.DiagnosticsAddress || !m.SecureServing || m.FilterProvider == nil {
			t.Errorf("metrics %+v: want them on %q over HTTPS behind a filter", m, c.Diagnostics.DiagnosticsAddress)
		}
		if options.HealthProbeBindAddress != c.HealthAddr {
			t.Errorf("probes on %q, want %q", options.HealthProbeBindAddress, c.HealthAddr)
		}
	}

	options, err := controller.ManagerOptions(controller.Config{SyncPeriod: 10 * time.Minute})
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
