// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"context"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

func TestAnInitializedControlPlaneIsNotRecreated(t *testing.T) {
	cluster, lcp, cp, r, issued := readyControlPlane(t, "xi")
	if err := k8s.Delete(t.Context(), cp); err != nil {
		t.Fatal(err)
	}

	stale := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if l, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				l.Status.Initialization.ControlPlaneInitialized = nil
			}
			return err
		},
	})
	stale.APIReader = r.APIReader
	passes(t, stale, lcp)
	passes(t, r, lcp)

	absent(t, cp)
	get(t, lcp)
	initialized(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason, "ControlPlane xi is gone"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason)
	kept := secretOf(cluster, secret.ClusterCA)
	get(t, kept)
	if !bytes.Equal(kept.Data[secret.TLSCrtDataName], issued.cert) {
		t.Error("the CA changed")
	}
}

func TestAControlPlaneVanishingBeforeTheApplyIsNotRecreated(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "tau")
	racing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsConflict(err) {
		t.Fatalf("the pass returned %v, want a conflict on the UID", err)
	}
	absent(t, cp)

	passes(t, r, lcp)
	absent(t, cp)
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason, "ControlPlane tau is gone"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason)
}

func TestAControlPlaneReplacedBeforeTheApplyIsLeftAlone(t *testing.T) {
	cluster, lcp, cp, _, _ := readyControlPlane(t, "tau-replaced")
	replacement := cp.DeepCopy()
	replacement.ObjectMeta = metav1.ObjectMeta{Namespace: cp.Namespace, Name: cp.Name, Finalizers: []string{lodiav1alpha1.DatastoreFinalizer}}
	racing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok && replacement.UID == "" {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
				if err := k8s.Create(ctx, replacement); err != nil {
					return err
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsInvalid(err) {
		t.Fatalf("the pass returned %v, want the UID refused", err)
	}
	got := controlPlaneOf(cluster)
	get(t, got)
	if got.UID != replacement.UID || got.Generation != replacement.Generation || metav1.GetControllerOf(got) != nil {
		t.Errorf("the replacement changed: uid %s, generation %d, owners %v", got.UID, got.Generation, got.OwnerReferences)
	}
}

func TestAControlPlaneReplacedBeforeTheDeleteIsLeftAlone(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "tau-delete")
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	replacement := cp.DeepCopy()
	replacement.ObjectMeta = metav1.ObjectMeta{Namespace: cp.Namespace, Name: cp.Name, Finalizers: []string{lodiav1alpha1.DatastoreFinalizer}}
	racing := direct(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if replacement.UID == "" {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
				if err := k8s.Create(ctx, replacement); err != nil {
					return err
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsConflict(err) {
		t.Fatalf("the pass returned %v, want a conflict on the UID", err)
	}
	got := controlPlaneOf(cluster)
	get(t, got)
	if got.UID != replacement.UID || !got.DeletionTimestamp.IsZero() {
		t.Errorf("the replacement changed: uid %s, deletion %v", got.UID, got.DeletionTimestamp)
	}

	passes(t, r, lcp)
	absent(t, lcp)
	get(t, got)
	if !got.DeletionTimestamp.IsZero() {
		t.Error("the replacement is being deleted")
	}
}

func behindTheCache(t *testing.T, hides func(client.Object) bool) *controller.Reconciler {
	t.Helper()
	live := direct(t, interceptor.Funcs{})
	stale := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if hides(obj) {
				return apierrors.NewNotFound(schema.GroupResource{Group: lodiav1alpha1.GroupVersion.Group, Resource: "controlplanes"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	stale.APIReader = live.APIReader
	return stale
}

func isControlPlane(obj client.Object) bool {
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		return o.GetKind() == "ControlPlane"
	case *lodiav1alpha1.ControlPlane:
		return true
	}
	return false
}

func TestAStaleCacheDoesNotHideALeftoverControlPlane(t *testing.T) {
	ns := namespace(t)
	_, lcp := create(t, ns, "stale")
	untilBlockMove(t, direct(t, interceptor.Funcs{}), lcp)
	leftover := &lodiav1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "stale"},
		Spec: lodiav1alpha1.ControlPlaneSpec{
			Version:   "v1.35.0",
			Datastore: spec().Datastore,
			Endpoint:  spec().Endpoint,
			Network: lodiav1alpha1.Network{
				ServiceCIDRs:  []lodiav1alpha1.CIDR{"10.128.0.0/12"},
				PodCIDRs:      []lodiav1alpha1.CIDR{"192.168.0.0/16"},
				ClusterDomain: "tenant.local",
			},
		},
	}
	if err := k8s.Create(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}

	if err := pass(t.Context(), behindTheCache(t, isControlPlane), lcp); err == nil {
		t.Fatal("the pass returned no error")
	}
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ObjectsControlledElsewhereReason, "ControlPlane stale exists without a controller"); err != nil {
		t.Error(err)
	}
	got := controlPlaneOf(&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "stale"}})
	get(t, got)
	if got.Spec.Version != "v1.35.0" || got.Generation != leftover.Generation || metav1.GetControllerOf(got) != nil {
		t.Errorf("the leftover changed: version %s, generation %d, owners %v", got.Spec.Version, got.Generation, got.OwnerReferences)
	}
}

func TestAStaleCacheDoesNotLetDeletionForgetTheControlPlane(t *testing.T) {
	_, lcp, cp, _, _ := readyControlPlane(t, "stale-delete")
	held := cp.DeepCopy()
	cp.Finalizers = append(cp.Finalizers, lodiav1alpha1.DatastoreFinalizer)
	if err := k8s.Patch(t.Context(), cp, client.MergeFrom(held)); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	passes(t, behindTheCache(t, isControlPlane), lcp)
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(lcp), lcp); err != nil || !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Fatalf("the LodiaControlPlane is gone or lost its finalizer while its ControlPlane exists: %v, finalizers %v", err, lcp.Finalizers)
	}
	get(t, cp)
	if cp.DeletionTimestamp.IsZero() {
		t.Error("the ControlPlane is not being deleted")
	}
}
