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
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestDeletingTheClusterConfirmsAndDeletesTheControlPlaneAndWaitsUntilItIsGone(t *testing.T) {
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

	startDeletingTheCluster(t, cluster)
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
	if got := cp.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; got != "true" {
		t.Errorf("%s is %q", lodiav1alpha1.DeletionConfirmedAnnotation, got)
	}
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

func startDeletingTheCluster(t *testing.T, cluster *clusterv1.Cluster) {
	t.Helper()
	before := cluster.DeepCopy()
	controllerutil.AddFinalizer(cluster, clusterv1.ClusterFinalizer)
	if err := k8s.Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
}

func refused(lcp *v1alpha1.LodiaControlPlane, cluster *clusterv1.Cluster) error {
	if err := ready(lcp, metav1.ConditionFalse, clusterv1.DeletingReason, fmt.Sprintf(
		"ControlPlane %s is not deleted, since the provider confirms its delete only while Cluster %s is being deleted: delete the Cluster, or confirm the ControlPlane by hand; the API server answered: ",
		cluster.Name, cluster.Name)); err != nil {
		return err
	}
	return ready(lcp, metav1.ConditionFalse, clusterv1.DeletingReason, fmt.Sprintf(
		"kubectl annotate controlplane %s --namespace %s --overwrite %s=true", cluster.Name, cluster.Namespace, lodiav1alpha1.DeletionConfirmedAnnotation))
}

func TestALodiaControlPlaneDeletedAloneLeavesTheControlPlaneAndReportsTheRefusal(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "alone")
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		passes(t, r, lcp)
		get(t, lcp)
		if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
			t.Fatalf("finalizers %v", lcp.Finalizers)
		}
		get(t, cp)
		if !cp.DeletionTimestamp.IsZero() {
			t.Fatal("the ControlPlane is being deleted")
		}
		if got, ok := cp.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; ok {
			t.Fatalf("the provider confirmed the ControlPlane: %q", got)
		}
	}
	if err := refused(lcp, cluster); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, clusterv1.DeletingReason)
}

func TestARefusedDeleteGoesOnOnceTheClusterIsDeletedOrTheControlPlaneConfirmed(t *testing.T) {
	for name, remedy := range map[string]func(*testing.T, *clusterv1.Cluster){
		"the Cluster deleted": startDeletingTheCluster,
		"the ControlPlane confirmed by hand": func(t *testing.T, cluster *clusterv1.Cluster) {
			cp := controlPlaneOf(cluster)
			get(t, cp)
			before := cp.DeepCopy()
			metav1.SetMetaDataAnnotation(&cp.ObjectMeta, lodiav1alpha1.DeletionConfirmedAnnotation, "true")
			if err := k8s.Patch(t.Context(), cp, client.MergeFrom(before)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			startManager(t, ns)
			cluster, lcp := create(t, ns, "refused")
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
				get(t, lcp)
				return refused(lcp, cluster)
			})
			settled(t, lcp)

			remedy(t, cluster)
			eventually(t, func() error {
				get(t, cp)
				if cp.DeletionTimestamp.IsZero() {
					return errors.New("the ControlPlane is not being deleted")
				}
				get(t, lcp)
				return ready(lcp, metav1.ConditionFalse, clusterv1.DeletingReason, "waiting for ControlPlane refused to be deleted")
			})
		})
	}
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

func TestTheControlPlaneIsConfirmedBeforeTheFinalizerGoesOnceTheClusterIsGone(t *testing.T) {
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
	if got := cp.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; got != "true" {
		t.Errorf("%s is %q", lodiav1alpha1.DeletionConfirmedAnnotation, got)
	}
	if err := k8s.Delete(t.Context(), cp, client.Preconditions{UID: &cp.UID}); err != nil {
		t.Errorf("the garbage collector's delete: %v", err)
	}
}

func TestAFailedConfirmationKeepsTheFinalizerOnceTheClusterIsGone(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "phi-confirm")
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("etcdserver: leader changed")
	failing := direct(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if isControlPlane(obj) {
				return failure
			}
			return c.Patch(ctx, obj, patch, opts...)
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
	if got, ok := cp.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; ok {
		t.Errorf("%s is %q", lodiav1alpha1.DeletionConfirmedAnnotation, got)
	}
	passes(t, r, lcp)
	absent(t, lcp)
}

func TestADeletedLodiaControlPlaneWithoutTheOwnerReferenceWaitsForIt(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "phi-ref")
	startDeletingTheCluster(t, cluster)
	owners := lcp.OwnerReferences
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.OwnerReferences = nil })
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		passes(t, r, lcp)
		get(t, lcp)
		if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
			t.Fatalf("finalizers %v", lcp.Finalizers)
		}
		get(t, cp)
		if _, ok := cp.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; ok || !cp.DeletionTimestamp.IsZero() {
			t.Fatalf("the ControlPlane was confirmed or deleted: annotations %v, deletion %v", cp.Annotations, cp.DeletionTimestamp)
		}
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.OwnerReferences = owners })
	passes(t, r, lcp)
	absent(t, cp)
}

func TestAFailedFinalizerRemovalIsRetried(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "phi-failed")
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("etcdserver: leader changed")
	failing := direct(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				return failure
			}
			return c.Patch(ctx, obj, patch, opts...)
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
	if got := cp.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; got != "true" {
		t.Errorf("%s is %q", lodiav1alpha1.DeletionConfirmedAnnotation, got)
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
	cluster, lcp, cp, r, _ := readyControlPlane(t, "read-error")
	startDeletingTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	unavailable := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if isControlPlane(obj) {
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

func TestAForbiddenDeleteWhileTheClusterIsDeletedIsReturnedForARetry(t *testing.T) {
	cluster, lcp, _, _, _ := readyControlPlane(t, "forbidden-deleting")
	startDeletingTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	forbidden := direct(t, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "lodia.io", Resource: "controlplanes"}, cluster.Name, nil)
		},
	})
	if err := pass(t.Context(), forbidden, lcp); !apierrors.IsForbidden(err) {
		t.Fatalf("the pass returned %v, want the refusal returned for a retry", err)
	}
}

func TestAGoneClusterLeavesAControlPlaneOfAnotherOwnerUnconfirmedAndReleasesTheFinalizer(t *testing.T) {
	ns := namespace(t)
	_, foreign := leftoverOfAnotherOwner(t, ns, "foreign-gone")
	cluster, lcp := create(t, ns, "foreign-gone")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Fatalf("finalizers %v", lcp.Finalizers)
	}
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	absent(t, lcp)
	get(t, foreign)
	if _, ok := foreign.Annotations[lodiav1alpha1.DeletionConfirmedAnnotation]; ok {
		t.Error("the ControlPlane of another owner was confirmed")
	}
}
