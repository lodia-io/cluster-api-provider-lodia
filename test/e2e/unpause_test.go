// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

// requireUnpaused undoes what a move that clusterctl began leaves, the Cluster
// paused, waits until the Cluster and the LodiaControlPlane report Paused
// False, which only a controller that has seen the Cluster unpaused sets, and
// then reads Available: both controllers stop at Paused while it is True, so
// Available stays what it was before the move, and says nothing until they run.
func requireUnpaused(ctx context.Context, c client.Client, key client.ObjectKey, bound, interval time.Duration) error {
	paused := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if err := c.Patch(ctx, paused, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"paused":null}}`))); err != nil {
		return err
	}
	var last error
	err := wait.PollUntilContextTimeout(ctx, interval, bound, true, func(ctx context.Context) (bool, error) {
		cluster, lcp := &clusterv1.Cluster{}, &v1alpha1.LodiaControlPlane{}
		last = errors.Join(c.Get(ctx, key, cluster), c.Get(ctx, key, lcp))
		if last == nil {
			last = errors.Join(pausedFalse("Cluster", cluster.Status.Conditions), pausedFalse("LodiaControlPlane", lcp.Status.Conditions))
		}
		return last == nil, nil
	})
	if err != nil {
		return fmt.Errorf("after %s, Paused is not False on both: %w", bound, errors.Join(err, last))
	}
	cluster := &clusterv1.Cluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return err
	}
	if available := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterAvailableCondition); available == nil || available.Status != metav1.ConditionTrue {
		return fmt.Errorf("Available is %+v after the Cluster was unpaused", available)
	}
	return nil
}

func pausedFalse(kind string, conditions []metav1.Condition) error {
	if paused := meta.FindStatusCondition(conditions, clusterv1.PausedCondition); paused == nil || paused.Status != metav1.ConditionFalse {
		return fmt.Errorf("the Paused of the %s is %+v", kind, paused)
	}
	return nil
}

var unpausedKey = client.ObjectKey{Namespace: "capi-join", Name: "w"}

func conditionsOf(paused, available metav1.ConditionStatus) []metav1.Condition {
	return []metav1.Condition{
		{Type: clusterv1.PausedCondition, Status: paused, Reason: "Reason", Message: "message"},
		{Type: clusterv1.AvailableCondition, Status: available, Reason: "Reason", Message: "message"},
	}
}

func objectsAfterAMove(clusterPaused, lcpPaused, available metav1.ConditionStatus) client.WithWatch {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: unpausedKey.Namespace, Name: unpausedKey.Name}, Spec: clusterv1.ClusterSpec{Paused: new(true)}}
	cluster.Status.Conditions = conditionsOf(clusterPaused, available)
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: unpausedKey.Namespace, Name: unpausedKey.Name}}
	lcp.Status.Conditions = conditionsOf(lcpPaused, metav1.ConditionTrue)
	return fake.NewClientBuilder().WithScheme(controller.Scheme()).WithObjects(cluster, lcp).Build()
}

func TestUnpausingClearsThePauseOfTheMoveAndWaitsForBothControllers(t *testing.T) {
	const observedAfter = 3
	var reads atomic.Int32
	c := objectsAfterAMove(metav1.ConditionTrue, metav1.ConditionTrue, metav1.ConditionTrue)
	observing := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if reads.Add(1) > observedAfter {
				switch o := obj.(type) {
				case *clusterv1.Cluster:
					o.Status.Conditions = conditionsOf(metav1.ConditionFalse, metav1.ConditionTrue)
				case *v1alpha1.LodiaControlPlane:
					o.Status.Conditions = conditionsOf(metav1.ConditionFalse, metav1.ConditionTrue)
				}
			}
			return nil
		},
	})

	if err := requireUnpaused(t.Context(), observing, unpausedKey, 5*time.Second, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n := reads.Load(); n <= observedAfter {
		t.Errorf("returned after %d reads, before the controllers had observed the unpause", n)
	}
	cluster := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), unpausedKey, cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Spec.Paused != nil {
		t.Errorf("spec.paused is %v, want it dropped as clusterctl drops it", *cluster.Spec.Paused)
	}
}

func TestUnpausingFailsWhileEitherControllerReportsPaused(t *testing.T) {
	cases := []struct {
		name                     string
		clusterPaused, lcpPaused metav1.ConditionStatus
		available                metav1.ConditionStatus
		want                     string
	}{
		{"both unpaused and Available", metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, ""},
		{"the Cluster still paused", metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue, "the Paused of the Cluster"},
		{"the LodiaControlPlane still paused", metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionTrue, "the Paused of the LodiaControlPlane"},
		{"both still paused", metav1.ConditionTrue, metav1.ConditionTrue, metav1.ConditionTrue, "the Paused of the Cluster"},
		{"unpaused and not Available", metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionFalse, "Available is"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireUnpaused(t.Context(), objectsAfterAMove(c.clusterPaused, c.lcpPaused, c.available), unpausedKey, 100*time.Millisecond, 10*time.Millisecond)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestUnpausingFailsWithoutACluster(t *testing.T) {
	err := requireUnpaused(t.Context(), fake.NewClientBuilder().WithScheme(controller.Scheme()).Build(), unpausedKey, 100*time.Millisecond, 10*time.Millisecond)
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("want a NotFound, got %v", err)
	}
}
