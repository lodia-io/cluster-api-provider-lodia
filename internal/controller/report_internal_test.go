// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

type verdict struct {
	Status          metav1.ConditionStatus
	Reason, Message string
}

type reported struct {
	Endpoint         clusterv1.APIEndpoint
	Initialized      *bool
	Version          string
	Versions         []string
	Ready, Available verdict
}

func reportedBy(lcp *v1alpha1.LodiaControlPlane) reported {
	out := reported{Endpoint: lcp.Spec.ControlPlaneEndpoint, Initialized: lcp.Status.Initialization.ControlPlaneInitialized, Version: lcp.Status.Version}
	for _, v := range lcp.Status.Versions {
		out.Versions = append(out.Versions, v.Version)
	}
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition); c != nil {
		out.Ready = verdict{c.Status, c.Reason, c.Message}
	}
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.AvailableCondition); c != nil {
		out.Available = verdict{c.Status, c.Reason, c.Message}
	}
	return out
}

func TestTheControlPlaneStatusIsReportedToTheLodiaControlPlane(t *testing.T) {
	apiServer := &lodiav1alpha1.EndpointStatus{APIServer: lodiav1alpha1.HostPort{Host: "203.0.113.10", Port: 31443}}
	condition := func(conditionType string, status metav1.ConditionStatus, reason, message string, generation int64) metav1.Condition {
		return metav1.Condition{Type: conditionType, Status: status, Reason: reason, Message: message, ObservedGeneration: generation}
	}
	ready := func(generation int64) metav1.Condition {
		return condition(lodiav1alpha1.ConditionReady, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, "every readiness condition is True", generation)
	}
	reachable := func(generation int64) metav1.Condition {
		return condition(lodiav1alpha1.ConditionReachable, metav1.ConditionTrue, lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz", generation)
	}
	trueVerdict := func(reason, message string) verdict { return verdict{metav1.ConditionTrue, reason, message} }
	notYetReported := verdict{metav1.ConditionUnknown, conditions.NotYetReportedReason, ""}
	initialized := new(true)
	previous := &v1alpha1.LodiaControlPlane{
		Spec: v1alpha1.LodiaControlPlaneSpec{ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "198.51.100.1", Port: 6443}},
		Status: v1alpha1.LodiaControlPlaneStatus{
			Initialization: v1alpha1.LodiaControlPlaneInitializationStatus{ControlPlaneInitialized: initialized},
			Version:        "v1.35.8",
			Versions:       []clusterv1.StatusVersion{{Version: "v1.35.8"}},
		},
	}

	for _, c := range []struct {
		name   string
		before *v1alpha1.LodiaControlPlane
		status lodiav1alpha1.ControlPlaneStatus
		copied bool
		want   reported
	}{
		{
			name: "nothing reported yet",
			want: reported{Ready: notYetReported, Available: notYetReported},
		},
		{
			name:   "nothing reported yet keeps what was reported before",
			before: previous,
			want: reported{
				Endpoint: previous.Spec.ControlPlaneEndpoint, Initialized: initialized, Version: "v1.35.8", Versions: []string{"v1.35.8"},
				Ready: notYetReported, Available: notYetReported,
			},
		},
		{
			name:   "ready at the current generation with both Secrets copied",
			status: lodiav1alpha1.ControlPlaneStatus{Endpoint: apiServer, Kubernetes: &lodiav1alpha1.KubernetesStatus{Version: "v1.35.8"}, Conditions: []metav1.Condition{ready(2), reachable(2)}},
			copied: true,
			want: reported{
				Endpoint: clusterv1.APIEndpoint{Host: "203.0.113.10", Port: 31443}, Initialized: initialized, Version: "v1.35.8", Versions: []string{"v1.35.8"},
				Ready:     trueVerdict(lodiav1alpha1.ReasonReady, "every readiness condition is True"),
				Available: trueVerdict(lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz"),
			},
		},
		{
			name:   "ready with a Secret not yet copied",
			status: lodiav1alpha1.ControlPlaneStatus{Conditions: []metav1.Condition{ready(2), reachable(2)}},
			want: reported{
				Ready:     trueVerdict(lodiav1alpha1.ReasonReady, "every readiness condition is True"),
				Available: trueVerdict(lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz"),
			},
		},
		{
			name:   "ready for an older generation initializes and holds Ready back, but not Available",
			status: lodiav1alpha1.ControlPlaneStatus{Conditions: []metav1.Condition{ready(1), reachable(1)}},
			copied: true,
			want: reported{
				Initialized: initialized,
				Ready:       verdict{metav1.ConditionUnknown, lodiav1alpha1.ReasonLatestGenerationNotObserved, "ControlPlane cp reports Ready for generation 1 of 2"},
				Available:   trueVerdict(lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz"),
			},
		},
		{
			name: "not ready does not initialize",
			status: lodiav1alpha1.ControlPlaneStatus{Conditions: []metav1.Condition{
				condition(lodiav1alpha1.ConditionReady, metav1.ConditionFalse, "WaitingForEtcd", "etcd has no quorum", 2),
				condition(lodiav1alpha1.ConditionReachable, metav1.ConditionFalse, lodiav1alpha1.ReasonExternalProbeFailed, "the public endpoint does not answer", 2),
			}},
			copied: true,
			want: reported{
				Ready:     verdict{metav1.ConditionFalse, "WaitingForEtcd", "etcd has no quorum"},
				Available: verdict{metav1.ConditionFalse, lodiav1alpha1.ReasonExternalProbeFailed, "the public endpoint does not answer"},
			},
		},
		{
			name:   "ready turning false does not clear initialized",
			before: previous,
			status: lodiav1alpha1.ControlPlaneStatus{Conditions: []metav1.Condition{
				condition(lodiav1alpha1.ConditionReady, metav1.ConditionFalse, "WaitingForEtcd", "etcd has no quorum", 2),
				reachable(2),
			}},
			copied: true,
			want: reported{
				Endpoint: previous.Spec.ControlPlaneEndpoint, Initialized: initialized, Version: "v1.35.8", Versions: []string{"v1.35.8"},
				Ready:     verdict{metav1.ConditionFalse, "WaitingForEtcd", "etcd has no quorum"},
				Available: trueVerdict(lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz"),
			},
		},
		{
			name:   "the version follows Lodia's once reported",
			before: previous,
			status: lodiav1alpha1.ControlPlaneStatus{Kubernetes: &lodiav1alpha1.KubernetesStatus{Version: "v1.35.9"}, Conditions: []metav1.Condition{ready(2), reachable(2)}},
			copied: true,
			want: reported{
				Endpoint: previous.Spec.ControlPlaneEndpoint, Initialized: initialized, Version: "v1.35.9", Versions: []string{"v1.35.9"},
				Ready:     trueVerdict(lodiav1alpha1.ReasonReady, "every readiness condition is True"),
				Available: trueVerdict(lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz"),
			},
		},
		{
			name:   "an empty version is not a report",
			before: previous,
			status: lodiav1alpha1.ControlPlaneStatus{Kubernetes: &lodiav1alpha1.KubernetesStatus{}, Conditions: []metav1.Condition{ready(2), reachable(2)}},
			copied: true,
			want: reported{
				Endpoint: previous.Spec.ControlPlaneEndpoint, Initialized: initialized, Version: "v1.35.8", Versions: []string{"v1.35.8"},
				Ready:     trueVerdict(lodiav1alpha1.ReasonReady, "every readiness condition is True"),
				Available: trueVerdict(lodiav1alpha1.ReasonReachable, "the public endpoint answers /readyz"),
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			lcp := &v1alpha1.LodiaControlPlane{}
			if c.before != nil {
				lcp = c.before.DeepCopy()
			}
			content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&lodiav1alpha1.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "cp", Generation: 2},
				Status:     c.status,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := report(lcp, &unstructured.Unstructured{Object: content}, c.copied); err != nil {
				t.Fatal(err)
			}
			got := reportedBy(lcp)
			if c.want.Ready.Message == "" {
				got.Ready.Message = ""
			}
			if c.want.Available.Message == "" {
				got.Available.Message = ""
			}
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Errorf("reported (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAWriteErrorIsReportedOnlyWhenRetryingCannotHelpUntilSomethingChanges(t *testing.T) {
	elsewhere := &controlledElsewhereError{kind: "Secret", object: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "alpha-ca"}}}
	resource := schema.GroupResource{Group: "lodia.io", Resource: "controlplanes"}
	for _, c := range []struct {
		name   string
		err    error
		reason string
	}{
		{"no error", nil, ""},
		{"an object controlled elsewhere", elsewhere, v1alpha1.ObjectsControlledElsewhereReason},
		{"an object controlled elsewhere beside another error", errors.Join(errors.New("first"), elsewhere), v1alpha1.ObjectsControlledElsewhereReason},
		{"an invalid object", apierrors.NewInvalid(schema.GroupKind{Group: "lodia.io", Kind: "ControlPlane"}, "alpha", nil), v1alpha1.RefusedReason},
		{"a forbidden write", apierrors.NewForbidden(resource, "alpha", errors.New("the namespace is terminating")), v1alpha1.RefusedReason},
		{"a conflict", apierrors.NewConflict(resource, "alpha", errors.New("the UID changed")), ""},
		{"an unavailable server", apierrors.NewServiceUnavailable("the API server is restarting"), ""},
		{"an internal error", apierrors.NewInternalError(errors.New("etcdserver: leader changed")), ""},
		{"a plain error", errors.New("boom"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			lcp := &v1alpha1.LodiaControlPlane{}
			if err := reportWriteError(lcp, c.err); !errors.Is(err, c.err) {
				t.Errorf("returned %v, want %v", err, c.err)
			}
			got := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition)
			switch {
			case c.reason == "" && got != nil:
				t.Errorf("Ready is %+v, want it unset", got)
			case c.reason != "" && got == nil:
				t.Errorf("Ready is unset, want %s", c.reason)
			case c.reason != "" && (got.Status != metav1.ConditionFalse || got.Reason != c.reason || got.Message != c.err.Error()):
				t.Errorf("Ready is %s %s %q, want False %s %q", got.Status, got.Reason, got.Message, c.reason, c.err.Error())
			}
		})
	}
}

func TestAnObjectControlledElsewhereIsNamedWithItsController(t *testing.T) {
	owned := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name:            "alpha",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ConfigMap", Name: "previous", UID: "abc", Controller: new(true)}},
	}}
	notControlled := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name:            "alpha",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ConfigMap", Name: "bystander", UID: "def"}},
	}}
	for _, c := range []struct {
		name   string
		kind   string
		object client.Object
		want   string
	}{
		{"a controller", "ControlPlane", owned, "ControlPlane alpha is controlled by ConfigMap previous (uid abc)"},
		{"no controller", "Secret", &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}, "Secret alpha exists without a controller"},
		{"an owner that is not a controller", "Secret", notControlled, "Secret alpha exists without a controller"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (&controlledElsewhereError{kind: c.kind, object: c.object}).Error(); got != c.want {
				t.Errorf("message %q, want %q", got, c.want)
			}
		})
	}
}

func TestOnlyAClusterOfTheProvidersKindWakesALodiaControlPlane(t *testing.T) {
	clusterWith := func(group, kind string) *clusterv1.Cluster {
		cluster := &clusterv1.Cluster{}
		cluster.Namespace = "tenants"
		cluster.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: group, Kind: kind, Name: "alpha-cp"}
		return cluster
	}
	for _, c := range []struct {
		name    string
		cluster *clusterv1.Cluster
		want    []reconcile.Request
	}{
		{"a LodiaControlPlane", clusterWith(v1alpha1.GroupVersion.Group, "LodiaControlPlane"), []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: "tenants", Name: "alpha-cp"}}}},
		{"another kind of the group", clusterWith(v1alpha1.GroupVersion.Group, "LodiaControlPlaneTemplate"), nil},
		{"the kind of another group", clusterWith("controlplane.cluster.x-k8s.io", "LodiaControlPlane"), nil},
		{"no control plane", &clusterv1.Cluster{}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if diff := cmp.Diff(c.want, lodiaControlPlaneOf(c.cluster)); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}
