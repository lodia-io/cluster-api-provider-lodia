// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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

func setClusterPaused(ctx context.Context, c client.Client, key client.ObjectKey, paused bool) error {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	value := "null"
	if paused {
		value = "true"
	}
	return c.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"paused":%s}}`, value)))
}

func reportsPaused(kind string, conditions []metav1.Condition) error {
	if paused := meta.FindStatusCondition(conditions, clusterv1.PausedCondition); paused == nil || paused.Status != metav1.ConditionTrue {
		return fmt.Errorf("the Paused of the %s is %+v", kind, paused)
	}
	return nil
}

// whilePaused waits for Paused True on the Cluster and the LodiaControlPlane,
// which only a controller that has seen the pause writes, before it runs act.
func whilePaused(ctx context.Context, c client.Client, key client.ObjectKey, bound, interval time.Duration, act func(context.Context) error) error {
	if err := setClusterPaused(ctx, c, key, true); err != nil {
		return err
	}
	var last error
	err := wait.PollUntilContextTimeout(ctx, interval, bound, true, func(ctx context.Context) (bool, error) {
		cluster, lcp := &clusterv1.Cluster{}, &v1alpha1.LodiaControlPlane{}
		last = errors.Join(c.Get(ctx, key, cluster), c.Get(ctx, key, lcp))
		if last == nil {
			last = errors.Join(reportsPaused("Cluster", cluster.Status.Conditions), reportsPaused("LodiaControlPlane", lcp.Status.Conditions))
		}
		return last == nil, nil
	})
	if err != nil {
		err = fmt.Errorf("after %s, Paused is not True on both: %w", bound, errors.Join(err, last))
	} else {
		err = act(ctx)
	}
	return errors.Join(err, setClusterPaused(ctx, c, key, false))
}

func TestAnActionRunsOnlyOnceBothControllersReportPausedAndTheClusterIsUnpausedAfterIt(t *testing.T) {
	cases := []struct {
		name          string
		clusterReads  int32
		lcpReports    bool
		actFails      bool
		wantActed     bool
		wantFailureIn string
	}{
		{"both report after a few reads", 3, true, false, true, ""},
		{"both report at the first read", 0, true, false, true, ""},
		{"the action fails", 0, true, true, true, "the action failed"},
		{"the LodiaControlPlane never reports", 0, false, false, false, "Paused is not True on both"},
		{"the Cluster never reports", -1, true, false, false, "Paused is not True on both"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: quickStartKey.Namespace, Name: quickStartKey.Name}}
			cluster.Status.Conditions, _ = conditionsOf(metav1.ConditionFalse, notReported, metav1.ConditionTrue, 0)
			lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: quickStartKey.Namespace, Name: quickStartKey.Name}}
			_, lcp.Status.Conditions = conditionsOf(notReported, metav1.ConditionFalse, notReported, 0)
			base := fake.NewClientBuilder().WithScheme(controller.Scheme()).WithObjects(cluster, lcp).Build()
			var reads atomic.Int32
			observing := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := w.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					switch o := obj.(type) {
					case *clusterv1.Cluster:
						if o.Spec.Paused != nil && *o.Spec.Paused && c.clusterReads >= 0 && reads.Add(1) > c.clusterReads {
							o.Status.Conditions, _ = conditionsOf(metav1.ConditionTrue, notReported, metav1.ConditionTrue, 0)
						}
					case *v1alpha1.LodiaControlPlane:
						if c.lcpReports && reads.Load() > c.clusterReads {
							_, o.Status.Conditions = conditionsOf(notReported, metav1.ConditionTrue, notReported, 0)
						}
					}
					return nil
				},
			})
			var acted bool
			err := whilePaused(t.Context(), observing, quickStartKey, 100*time.Millisecond, 5*time.Millisecond, func(ctx context.Context) error {
				acted = true
				paused := &clusterv1.Cluster{}
				if err := base.Get(ctx, quickStartKey, paused); err != nil {
					return err
				}
				if paused.Spec.Paused == nil || !*paused.Spec.Paused {
					t.Error("the action ran with the Cluster not paused")
				}
				if reads.Load() <= c.clusterReads {
					t.Errorf("the action ran after %d reads, before the controllers had reported the pause", reads.Load())
				}
				if c.actFails {
					return errors.New("the action failed")
				}
				return nil
			})
			requireFailure(t, err, c.wantFailureIn)
			if acted != c.wantActed {
				t.Errorf("the action ran: %v, want %v", acted, c.wantActed)
			}
			after := &clusterv1.Cluster{}
			if err := base.Get(t.Context(), quickStartKey, after); err != nil {
				t.Fatal(err)
			}
			if after.Spec.Paused != nil {
				t.Errorf("spec.paused is %v after the call, want it dropped", *after.Spec.Paused)
			}
		})
	}
}
