// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestDeletingWaitsUntilTheControlPlaneIsGone(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	cluster, lcp := create(t, ns, "gamma")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	before := cp.DeepCopy()
	cp.Finalizers = append(cp.Finalizers, lodiav1alpha1.DatastoreFinalizer)
	if err := k8s.Patch(t.Context(), cp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}

	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		get(t, cp)
		if cp.DeletionTimestamp.IsZero() {
			return errors.New("the ControlPlane is not being deleted")
		}
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, clusterv1.DeletingReason, "waiting for ControlPlane gamma to be deleted")
	})
	available(t, lcp, metav1.ConditionFalse, clusterv1.DeletingReason)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}

	before = cp.DeepCopy()
	cp.Finalizers = nil
	if err := k8s.Patch(t.Context(), cp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(lcp), lcp); err == nil {
			return fmt.Errorf("the LodiaControlPlane is still there with finalizers %v", lcp.Finalizers)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
	absent(t, cp)
}

func deleteTheCluster(t *testing.T, cluster *clusterv1.Cluster) {
	t.Helper()
	if err := k8s.Delete(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster); !apierrors.IsNotFound(err) {
			return fmt.Errorf("the Cluster: %w", err)
		}
		return nil
	})
}

func TestDeletionCompletesOnceTheClusterIsGone(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "phi")
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)
	absent(t, lcp)
	get(t, cp)
	if !cp.DeletionTimestamp.IsZero() {
		t.Error("the provider deleted the ControlPlane the garbage collector owns")
	}
}

func TestDeletionCompletesOnceTheOwnerReferenceIsGone(t *testing.T) {
	_, lcp, _, r, _ := readyControlPlane(t, "phi-ref")
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.OwnerReferences = nil })
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)
	absent(t, lcp)
}

func TestAFailedFinalizerRemovalIsRetried(t *testing.T) {
	cluster, lcp, _, r, _ := readyControlPlane(t, "phi-failed")
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("etcdserver: leader changed")
	failing := direct(t, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return failure
		},
	})

	if err := pass(t.Context(), failing, lcp); !errors.Is(err, failure) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
	passes(t, r, lcp)
	absent(t, lcp)
}

func TestATransientClusterReadWhileDeletingKeepsTheFinalizer(t *testing.T) {
	_, lcp, _, _, _ := readyControlPlane(t, "phi-read")
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	unavailable := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*clusterv1.Cluster); ok {
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
}

func TestAGoneClusterKeepsTheFinalizerUntilDeletion(t *testing.T) {
	cluster, lcp, _, r, _ := readyControlPlane(t, "phi-kept")
	deleteTheCluster(t, cluster)

	if err := pass(t.Context(), r, lcp); !apierrors.IsNotFound(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
}

func TestAReadErrorWhileDeletingKeepsTheFinalizer(t *testing.T) {
	_, lcp, cp, r, _ := readyControlPlane(t, "read-error")
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	unavailable := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*lodiav1alpha1.ControlPlane); ok {
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
	get(t, cp)
	if !cp.DeletionTimestamp.IsZero() {
		t.Error("the ControlPlane is being deleted")
	}

	passes(t, r, lcp)
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); !apierrors.IsNotFound(err) {
			return fmt.Errorf("the ControlPlane: %w", err)
		}
		return nil
	})
	passes(t, r, lcp)
	absent(t, lcp)
}

func TestAFailedDeleteIsReturnedAndKeepsTheFinalizer(t *testing.T) {
	_, lcp, cp, _, _ := readyControlPlane(t, "delete-error")
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("the API server refused the delete")
	failing := direct(t, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return failure
		},
	})

	if err := pass(t.Context(), failing, lcp); !errors.Is(err, failure) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
	get(t, cp)
	if !cp.DeletionTimestamp.IsZero() {
		t.Error("the ControlPlane is being deleted")
	}
}

func TestNoOwnerReferenceAndNoDeletionWaits(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "no-owner")
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.OwnerReferences = nil })

	passes(t, direct(t, interceptor.Funcs{}), lcp)
	get(t, lcp)
	if len(lcp.Finalizers) != 0 {
		t.Errorf("finalizers %v, want none before an owner reference", lcp.Finalizers)
	}
	absent(t, controlPlaneOf(cluster))
}
