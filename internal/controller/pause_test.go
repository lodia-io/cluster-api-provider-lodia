// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestDeletionWaitsWhilePaused(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "paused-delete")
	unpaused := cluster.DeepCopy()
	cluster.Spec.Paused = new(true)
	if err := k8s.Patch(t.Context(), cluster, client.MergeFrom(unpaused)); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		passes(t, r, lcp)
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); err != nil || !cp.DeletionTimestamp.IsZero() {
			t.Fatalf("the ControlPlane was deleted while paused: %v, deletion %v", err, cp.DeletionTimestamp)
		}
		heldByThePause(t, lcp)
	}

	paused := cluster.DeepCopy()
	cluster.Spec.Paused = new(false)
	if err := k8s.Patch(t.Context(), cluster, client.MergeFrom(paused)); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	passes(t, r, lcp)
	absent(t, cp)
}

func TestThePausedAnnotationHoldsDeletionOnceTheClusterIsGone(t *testing.T) {
	cluster, lcp, _, r, _ := readyControlPlane(t, "phi-paused")
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) {
		metav1.SetMetaDataAnnotation(&l.ObjectMeta, clusterv1.PausedAnnotation, "")
	})
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		passes(t, r, lcp)
		heldByThePause(t, lcp)
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { delete(l.Annotations, clusterv1.PausedAnnotation) })
	passes(t, r, lcp)
	passes(t, direct(t, interceptor.Funcs{}), lcp)
	absent(t, lcp)
}

func TestThePausedAnnotationHoldsDeletionOnceTheOwnerReferenceIsGone(t *testing.T) {
	_, lcp, _, r, _ := readyControlPlane(t, "phi-ref-paused")
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) {
		metav1.SetMetaDataAnnotation(&l.ObjectMeta, clusterv1.PausedAnnotation, "")
		l.OwnerReferences = nil
	})
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		passes(t, r, lcp)
		heldByThePause(t, lcp)
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { delete(l.Annotations, clusterv1.PausedAnnotation) })
	passes(t, r, lcp)
	passes(t, r, lcp)
	absent(t, lcp)
}

func TestAFailedPausedWriteKeepsTheFinalizerOnceTheClusterIsGone(t *testing.T) {
	cluster, lcp, _, r, _ := readyControlPlane(t, "phi-paused-write")
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) {
		metav1.SetMetaDataAnnotation(&l.ObjectMeta, clusterv1.PausedAnnotation, "")
	})
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	heldByThePause(t, lcp)
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { delete(l.Annotations, clusterv1.PausedAnnotation) })

	failure := errors.New("etcdserver: leader changed")
	failing := direct(t, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return failure
		},
	})
	if err := pass(t.Context(), failing, lcp); !errors.Is(err, failure) {
		t.Fatalf("the pass returned %v", err)
	}
	heldByThePause(t, lcp)

	slow := direct(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := pass(ctx, slow, lcp); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the pass returned %v", err)
	}
	heldByThePause(t, lcp)

	passes(t, r, lcp)
	passes(t, r, lcp)
	absent(t, lcp)
}

func heldByThePause(t *testing.T, lcp *v1alpha1.LodiaControlPlane) {
	t.Helper()
	get(t, lcp)
	if !controllerutil.ContainsFinalizer(lcp, v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
	if c := condition(lcp, clusterv1.PausedCondition); c == nil || c.Status != metav1.ConditionTrue || c.Reason != clusterv1.PausedReason {
		t.Errorf("Paused is %+v", c)
	}
}

func TestPauseStopsTheProvider(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	cluster, lcp := create(t, ns, "delta", func(c *clusterv1.Cluster, _ *v1alpha1.LodiaControlPlane) { c.Spec.Paused = new(true) })
	paused := func(message string) func() error {
		return func() error {
			get(t, lcp)
			c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.PausedCondition)
			if c == nil || c.Status != metav1.ConditionTrue || c.Message != message {
				return fmt.Errorf("Paused is %+v", c)
			}
			return nil
		}
	}
	eventually(t, paused("Cluster spec.paused is set to true"))
	absent(t, controlPlaneOf(cluster))

	before := lcp.DeepCopy()
	metav1.SetMetaDataAnnotation(&lcp.ObjectMeta, clusterv1.PausedAnnotation, "")
	if err := k8s.Patch(t.Context(), lcp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	unpaused := cluster.DeepCopy()
	unpaused.Spec.Paused = new(false)
	if err := k8s.Patch(t.Context(), unpaused, client.MergeFrom(cluster)); err != nil {
		t.Fatal(err)
	}
	eventually(t, paused("LodiaControlPlane has the cluster.x-k8s.io/paused annotation"))
	absent(t, controlPlaneOf(cluster))

	before = lcp.DeepCopy()
	delete(lcp.Annotations, clusterv1.PausedAnnotation)
	if err := k8s.Patch(t.Context(), lcp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		return k8s.Get(t.Context(), client.ObjectKeyFromObject(cluster), controlPlaneOf(cluster))
	})
}

func settled(t *testing.T, lcp *v1alpha1.LodiaControlPlane) {
	t.Helper()
	var version string
	var since time.Time
	eventually(t, func() error {
		get(t, lcp)
		if condition(lcp, clusterv1.PausedCondition) == nil {
			return errors.New("no Paused condition yet")
		}
		if lcp.ResourceVersion != version {
			version, since = lcp.ResourceVersion, time.Now()
			return errors.New("still being written")
		}
		if time.Since(since) < 2*time.Second {
			return errors.New("not settled yet")
		}
		return nil
	})
}

func TestAClusterEventAloneReachesARunningManager(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	cluster, lcp := create(t, ns, "delta-event")
	eventually(t, func() error {
		return k8s.Get(t.Context(), client.ObjectKeyFromObject(cluster), controlPlaneOf(cluster))
	})
	settled(t, lcp)

	paused := cluster.DeepCopy()
	paused.Spec.Paused = new(true)
	if err := k8s.Patch(t.Context(), paused, client.MergeFrom(cluster)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		get(t, lcp)
		c := condition(lcp, clusterv1.PausedCondition)
		if c == nil || c.Status != metav1.ConditionTrue || c.Message != "Cluster spec.paused is set to true" {
			return fmt.Errorf("Paused is %+v", c)
		}
		return nil
	})
}
