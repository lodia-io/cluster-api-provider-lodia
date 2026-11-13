// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func setClusterPaused(ctx context.Context, c client.Client, key client.ObjectKey, paused bool) (int64, error) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	value := "null"
	if paused {
		value = "true"
	}
	err := c.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"paused":%s}}`, value)))
	return cluster.Generation, err
}

// unpauseBound is the unpause's own, since its step's bound may have ended: a
// Cluster left paused would hold every controller of the suite.
var unpauseBound = 30 * time.Second

// pauseCycle is the pause of steps 11, 19 and 21. It waits until both
// controllers report Paused True, runs act, unpauses by dropping spec.paused as
// clusterctl does, and requires that both controllers ran after it. Both stop
// at Paused while it is True, so the Paused False it waits for is a write of a
// controller that has read the Cluster unpaused, and not the False of before
// the pause: a LodiaControlPlane that misses the unpause stays True. The
// Cluster's Available is recomputed only by the full pass that follows, which
// stamps it with the Cluster's generation, so it has to be True at the
// generation that the unpause produced or a later one. The LodiaControlPlane's
// generation does not move with the Cluster's, so nothing beyond its Paused
// going True and then False shows that the provider resumed.
// The pause, both waits and act, which gets their context, share bound. The
// unpause has unpauseBound of its own, and no deadline of its caller's, so that
// it goes out when bound has ended.
// With pause false the Cluster is paused already, by a move that clusterctl
// began, and is left as it is when the first wait fails; with pause true it is
// unpaused whatever fails, since it was this call that paused it.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/paused/paused.go#L63-L90
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller.go#L171-L199
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_status.go#L92
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/conditions/setter.go#L84
func pauseCycle(ctx context.Context, c client.Client, key client.ObjectKey, bound, interval time.Duration, pause bool, act func(context.Context) error) error {
	bounded, cancel := endAfter(ctx, bound)
	defer cancel()
	var err error
	if pause {
		_, err = setClusterPaused(bounded, c, key, true)
		err = cutShort(bounded, err)
	}
	holding := pause
	if err == nil {
		if err = pollUntil(bounded, interval, func(ctx context.Context) error {
			cluster, lcp, err := readBoth(ctx, c, key)
			if err != nil {
				return err
			}
			return errors.Join(requirePaused("Cluster", cluster.Status.Conditions, metav1.ConditionTrue), requirePaused("LodiaControlPlane", lcp.Status.Conditions, metav1.ConditionTrue))
		}); err != nil {
			err = fmt.Errorf("after %s, Paused is not True on both: %w", bound, err)
		} else {
			holding = true
			if act != nil {
				err = cutShort(bounded, act(bounded))
			}
		}
	}
	if !holding {
		return err
	}
	unpausing, stop := context.WithTimeout(context.WithoutCancel(ctx), unpauseBound)
	defer stop()
	generation, unpauseErr := setClusterPaused(unpausing, c, key, false)
	if err != nil || unpauseErr != nil {
		return errors.Join(err, unpauseErr)
	}
	if err := pollUntil(bounded, interval, func(ctx context.Context) error {
		cluster, lcp, err := readBoth(ctx, c, key)
		if err != nil {
			return err
		}
		return errors.Join(
			requirePaused("Cluster", cluster.Status.Conditions, metav1.ConditionFalse),
			requirePaused("LodiaControlPlane", lcp.Status.Conditions, metav1.ConditionFalse),
			requireAvailableFrom(cluster.Status.Conditions, generation))
	}); err != nil {
		return fmt.Errorf("after the unpause, which made generation %d, both controllers did not run within %s: %w", generation, bound, err)
	}
	return nil
}

func whilePaused(ctx context.Context, c client.Client, key client.ObjectKey, bound, interval time.Duration, act func(context.Context) error) error {
	return pauseCycle(ctx, c, key, bound, interval, true, act)
}

func withUnpauseBound(t *testing.T, bound time.Duration) {
	t.Helper()
	was := unpauseBound
	unpauseBound = bound
	t.Cleanup(func() { unpauseBound = was })
}

func TestAnActionRunsOnlyOnceBothControllersReportPausedAndTheClusterIsUnpausedAfterItAndBothRunAgain(t *testing.T) {
	reportsAfter := func(n int) func(int) reported {
		return func(read int) reported {
			if read < n {
				return pausedByTheMove
			}
			return runAgain
		}
	}
	cases := []struct {
		name                     string
		before, after            func(int) reported
		reportedAt, recomputedAt int
		actFails                 bool
		wantActed                bool
		wantFailureIn            string
	}{
		{"both report after a few reads and run again after a few", func(n int) reported {
			if n < 3 {
				return beforeTheMove
			}
			return pausedByTheMove
		}, reportsAfter(2), 3, 2, false, true, ""},
		{"both report at the first read", always(pausedByTheMove), always(runAgain), 0, 0, false, true, ""},
		{"the action fails", always(pausedByTheMove), always(pausedByTheMove), 0, 0, true, true, "the action failed"},
		{"the LodiaControlPlane never reports", always(reported{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration - 1}), always(runAgain), 0, 0, false, false, "Paused is not True on both"},
		{"the Cluster never reports", always(reported{metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionTrue, pausedGeneration - 1}), always(runAgain), 0, 0, false, false, "Paused is not True on both"},
		{"neither controller runs after the unpause", always(pausedByTheMove), always(pausedByTheMove), 0, 0, false, true, "both controllers did not run"},
		{"the Cluster is still paused after the unpause", always(pausedByTheMove), always(reported{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue, unpausedGeneration}), 0, 0, false, true, "the Paused of the Cluster"},
		{"the LodiaControlPlane is still paused after the unpause", always(pausedByTheMove), always(reported{metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionTrue, unpausedGeneration}), 0, 0, false, true, "the Paused of the LodiaControlPlane"},
		{"the Available of the Cluster is the one of before the unpause", always(pausedByTheMove), always(reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration}), 0, 0, false, true, "True at generation 5 or later"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorldPausedAt(false, c.before, c.after)
			bound := 100 * time.Millisecond
			if c.wantFailureIn == "" {
				bound = 5 * time.Second
			}
			var acted bool
			err := whilePaused(t.Context(), w, unpausedKey, bound, 5*time.Millisecond, func(ctx context.Context) error {
				acted = true
				paused := &clusterv1.Cluster{}
				if err := w.Get(ctx, unpausedKey, paused); err != nil {
					return err
				}
				if paused.Spec.Paused == nil || !*paused.Spec.Paused {
					t.Error("the action ran with the Cluster not paused")
				}
				if w.readsBefore[0] <= c.reportedAt || w.readsBefore[1] <= c.reportedAt {
					t.Errorf("the action ran after %d reads of the Cluster and %d of the LodiaControlPlane, before both had reported the pause at the read %d", w.readsBefore[0], w.readsBefore[1], c.reportedAt)
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
			if err := w.Get(t.Context(), unpausedKey, after); err != nil {
				t.Fatal(err)
			}
			if after.Spec.Paused != nil {
				t.Errorf("spec.paused is %v after the call, want it dropped", *after.Spec.Paused)
			}
			if c.wantFailureIn == "" && w.readsAfter[0] <= c.recomputedAt {
				t.Errorf("returned after %d reads of the Cluster since the unpause, before the controllers had run again at the read %d", w.readsAfter[0], c.recomputedAt)
			}
		})
	}
}

func TestAPauseTakesOneBoundForBothWaitsAndTheAction(t *testing.T) {
	const (
		bound      = 800 * time.Millisecond
		reportedAt = 3 * bound / 4
	)
	start := time.Now()
	w := newWorldPausedAt(false, func(int) reported {
		if time.Since(start) < reportedAt {
			return beforeTheMove
		}
		return pausedByTheMove
	}, always(pausedByTheMove))
	err := whilePaused(t.Context(), w, unpausedKey, bound, 10*time.Millisecond, func(context.Context) error { return nil })
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "both controllers did not run") {
		t.Fatalf("want the failure of the second wait, got %v", err)
	}
	if limit := bound + bound/4; took > limit {
		t.Errorf("the waits took %s, more than %s: the second one has a bound of its own, and not what the first left of the %s that both share", took, limit, bound)
	}
}

func TestAPauseEndsAtItsBoundAndItsUnpauseAtItsOwnWhenTheClusterStopsAnswering(t *testing.T) {
	const bound, own, interval = 400 * time.Millisecond, 200 * time.Millisecond, 5 * time.Millisecond
	withUnpauseBound(t, own)
	cases := []struct {
		name         string
		silentAfter  string
		callersBound time.Duration
		act          func(context.Context, client.Client) error
		wantRemovals int32
	}{
		{"the Cluster does not answer at all", "the start", 0, func(ctx context.Context, c client.Client) error {
			return removeOwnerReferences(ctx, c, quickStartKey.Namespace)
		}, 0},
		{"the Cluster stops answering once it is paused", "the pause", 0, func(ctx context.Context, c client.Client) error {
			return removeOwnerReferences(ctx, c, quickStartKey.Namespace)
		}, 0},
		{"the Cluster stops answering at the first patch of the removal", "the first removal", 0, func(ctx context.Context, c client.Client) error {
			return removeOwnerReferences(ctx, c, quickStartKey.Namespace)
		}, 1},
		{"the action never returns", "", 0, func(ctx context.Context, _ client.Client) error {
			<-ctx.Done()
			return ctx.Err()
		}, 0},
		{"the action never returns and its caller's context ends first", "", bound / 2, func(ctx context.Context, _ client.Client) error {
			<-ctx.Done()
			return ctx.Err()
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hangs := func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
					return errors.New("the call was never ended by its context")
				}
			}
			var silent atomic.Bool
			var removals, unpauses atomic.Int32
			base := newQuickStartNamespace().client()
			observed := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if silent.Load() {
						return hangs(ctx)
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := w.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					switch o := obj.(type) {
					case *clusterv1.Cluster:
						o.Status.Conditions, _ = conditionsOf(metav1.ConditionTrue, notReported, notReported, 0)
					case *v1alpha1.LodiaControlPlane:
						_, o.Status.Conditions = conditionsOf(notReported, metav1.ConditionTrue, notReported, 0)
					}
					return nil
				},
				List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if silent.Load() {
						return hangs(ctx)
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					return w.List(ctx, list, opts...)
				},
				Patch: func(ctx context.Context, w client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					data, _ := patch.Data(obj)
					_, isCluster := obj.(*clusterv1.Cluster)
					unpause := isCluster && strings.Contains(string(data), "null")
					if unpause {
						unpauses.Add(1)
					}
					if silent.Load() {
						return hangs(ctx)
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					if !isCluster {
						removals.Add(1)
						if c.silentAfter == "the first removal" {
							silent.Store(true)
							return hangs(ctx)
						}
						return w.Patch(ctx, obj, patch, opts...)
					}
					if err := w.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if !unpause {
						silent.Store(c.silentAfter == "the pause")
					}
					return nil
				},
			})
			silent.Store(c.silentAfter == "the start")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.callersBound > 0 {
				ctx, cancel = context.WithTimeout(ctx, c.callersBound)
				defer cancel()
			}
			start := time.Now()
			err := whilePaused(ctx, observed, quickStartKey, bound, interval, func(ctx context.Context) error { return c.act(ctx, observed) })
			took := time.Since(start)
			if err == nil {
				t.Fatal("want a failure, got none")
			}
			wantCause := "context deadline exceeded after " + bound.String()
			if c.callersBound > 0 {
				wantCause = context.DeadlineExceeded.Error()
			}
			if !strings.Contains(err.Error(), wantCause) {
				t.Errorf("the failure does not say %q: %v", wantCause, err)
			}
			if want := min(bound, cmp.Or(c.callersBound, bound)); took < want {
				t.Errorf("the call ended after %s, before its bound %s", took, want)
			}
			if limit := bound + own + 400*time.Millisecond; took > limit {
				t.Errorf("the call took %s, more than %s: the bound %s and the unpause's own %s end it", took, limit, bound, own)
			}
			if got := removals.Load(); got != c.wantRemovals {
				t.Errorf("%d patches of the removal were tried, want %d: it stops at its first failure", got, c.wantRemovals)
			}
			if got := unpauses.Load(); got != 1 {
				t.Errorf("%d unpauses were tried, want one", got)
			}
			if c.silentAfter == "" {
				after := &clusterv1.Cluster{}
				requireNoError(t, base.Get(t.Context(), quickStartKey, after))
				if after.Spec.Paused != nil {
					t.Errorf("spec.paused is %v after the call, want it dropped although the bound had ended", *after.Spec.Paused)
				}
			}
		})
	}
}
