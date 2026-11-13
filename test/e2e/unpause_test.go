// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// paused, and requires that both controllers ran after it. Both stop at Paused
// while it is True, so it first waits for Paused True on the Cluster and the
// LodiaControlPlane: the Paused False it waits for after the unpause is then a
// write of a controller that has read the Cluster unpaused, not the False of
// before the move. The Cluster's Available is recomputed only by the full pass
// that follows, which stamps it with the Cluster's generation, so it has to be
// True at the generation that the unpause produced or a later one. The
// LodiaControlPlane's generation does not move with the Cluster's, so nothing
// beyond its Paused going True and then False shows that the provider resumed.
// Both waits share the bound.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/paused/paused.go#L63-L90
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller.go#L171-L199
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_status.go#L92
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/conditions/setter.go#L84
func requireUnpaused(ctx context.Context, c client.Client, key client.ObjectKey, bound, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	if err := pollUntil(ctx, interval, func(ctx context.Context) error {
		cluster, lcp, err := readBoth(ctx, c, key)
		if err != nil {
			return err
		}
		return errors.Join(requirePaused("Cluster", cluster.Status.Conditions, metav1.ConditionTrue), requirePaused("LodiaControlPlane", lcp.Status.Conditions, metav1.ConditionTrue))
	}); err != nil {
		return fmt.Errorf("the move did not leave Paused True on both within %s: %w", bound, err)
	}
	unpaused := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if err := c.Patch(ctx, unpaused, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"paused":null}}`))); err != nil {
		return err
	}
	if err := pollUntil(ctx, interval, func(ctx context.Context) error {
		cluster, lcp, err := readBoth(ctx, c, key)
		if err != nil {
			return err
		}
		return errors.Join(
			requirePaused("Cluster", cluster.Status.Conditions, metav1.ConditionFalse),
			requirePaused("LodiaControlPlane", lcp.Status.Conditions, metav1.ConditionFalse),
			requireAvailableFrom(cluster.Status.Conditions, unpaused.Generation))
	}); err != nil {
		return fmt.Errorf("after the unpause, which made generation %d, both controllers did not run within %s: %w", unpaused.Generation, bound, err)
	}
	return nil
}

func pollUntil(ctx context.Context, interval time.Duration, check func(context.Context) error) error {
	var last error
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		last = check(ctx)
		return last == nil, nil
	})
	return errors.Join(err, last)
}

func readBoth(ctx context.Context, c client.Client, key client.ObjectKey) (*clusterv1.Cluster, *v1alpha1.LodiaControlPlane, error) {
	cluster, lcp := &clusterv1.Cluster{}, &v1alpha1.LodiaControlPlane{}
	return cluster, lcp, errors.Join(c.Get(ctx, key, cluster), c.Get(ctx, key, lcp))
}

func requirePaused(kind string, conditions []metav1.Condition, status metav1.ConditionStatus) error {
	if paused := meta.FindStatusCondition(conditions, clusterv1.PausedCondition); paused == nil || paused.Status != status {
		return fmt.Errorf("the Paused of the %s is %+v, want %s", kind, paused, status)
	}
	return nil
}

func requireAvailableFrom(conditions []metav1.Condition, generation int64) error {
	if available := meta.FindStatusCondition(conditions, clusterv1.ClusterAvailableCondition); available == nil || available.Status != metav1.ConditionTrue || available.ObservedGeneration < generation {
		return fmt.Errorf("the Available of the Cluster is %+v, want True at generation %d or later", available, generation)
	}
	return nil
}

var unpausedKey = client.ObjectKey{Namespace: "capi-join", Name: "w"}

const (
	pausedGeneration   = 4
	unpausedGeneration = pausedGeneration + 1
	notReported        = metav1.ConditionStatus("")
)

type reported struct {
	clusterPaused, lcpPaused metav1.ConditionStatus
	available                metav1.ConditionStatus
	availableGeneration      int64
}

var (
	beforeTheMove   = reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration - 1}
	pausedByTheMove = reported{metav1.ConditionTrue, metav1.ConditionTrue, metav1.ConditionTrue, pausedGeneration - 1}
	runAgain        = reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, unpausedGeneration}
)

func conditionsOf(clusterPaused, lcpPaused, available metav1.ConditionStatus, generation int64) (cluster, lcp []metav1.Condition) {
	if clusterPaused != notReported {
		cluster = append(cluster, metav1.Condition{Type: clusterv1.PausedCondition, Status: clusterPaused, Reason: "Reason", Message: "message"})
	}
	if available != notReported {
		cluster = append(cluster, metav1.Condition{Type: clusterv1.AvailableCondition, Status: available, Reason: "Reason", Message: "message", ObservedGeneration: generation})
	}
	if lcpPaused != notReported {
		lcp = append(lcp, metav1.Condition{Type: clusterv1.PausedCondition, Status: lcpPaused, Reason: "Reason", Message: "message"})
	}
	return cluster, lcp
}

func always(r reported) func(int) reported { return func(int) reported { return r } }

type world struct {
	client.Client
	readsBefore, readsAfter [2]int
}

func newWorld(before, after func(int) reported) *world {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: unpausedKey.Namespace, Name: unpausedKey.Name}, Spec: clusterv1.ClusterSpec{Paused: new(true)}}
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: unpausedKey.Namespace, Name: unpausedKey.Name}}
	w := &world{}
	var unpaused bool
	w.Client = interceptor.NewClient(fake.NewClientBuilder().WithScheme(controller.Scheme()).WithObjects(cluster, lcp).Build(), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			obj.SetGeneration(unpausedGeneration)
			unpaused = true
			return nil
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			reads, state := &w.readsBefore, before
			if unpaused {
				reads, state = &w.readsAfter, after
			}
			switch o := obj.(type) {
			case *clusterv1.Cluster:
				r := state(reads[0])
				reads[0]++
				o.Status.Conditions, _ = conditionsOf(r.clusterPaused, r.lcpPaused, r.available, r.availableGeneration)
			case *v1alpha1.LodiaControlPlane:
				r := state(reads[1])
				reads[1]++
				_, o.Status.Conditions = conditionsOf(r.clusterPaused, r.lcpPaused, r.available, r.availableGeneration)
			}
			return nil
		},
	})
	return w
}

func TestUnpausingWaitsForBothControllersToReportThePauseBeforeItClearsIt(t *testing.T) {
	const reportedAt = 3
	w := newWorld(func(n int) reported {
		if n < reportedAt {
			return beforeTheMove
		}
		return pausedByTheMove
	}, always(runAgain))

	if err := requireUnpaused(t.Context(), w, unpausedKey, 5*time.Second, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if w.readsBefore[0] <= reportedAt || w.readsBefore[1] <= reportedAt {
		t.Errorf("the Cluster was unpaused after %d reads of it and %d of the LodiaControlPlane, before both had reported the pause at the read %d", w.readsBefore[0], w.readsBefore[1], reportedAt)
	}
}

func TestUnpausingClearsThePauseOfTheMoveAndWaitsForBothControllersToRunAgain(t *testing.T) {
	const pausedUntil, recomputedAt = 2, 5
	w := newWorld(always(pausedByTheMove), func(n int) reported {
		switch {
		case n < pausedUntil:
			return pausedByTheMove
		case n < recomputedAt:
			return reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration - 1}
		}
		return runAgain
	})

	if err := requireUnpaused(t.Context(), w, unpausedKey, 5*time.Second, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if w.readsAfter[0] <= recomputedAt {
		t.Errorf("returned after %d reads of the Cluster since the unpause, before its Available was recomputed at the read %d", w.readsAfter[0], recomputedAt)
	}
	cluster := &clusterv1.Cluster{}
	if err := w.Get(t.Context(), unpausedKey, cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Spec.Paused != nil {
		t.Errorf("spec.paused is %v, want it dropped as clusterctl drops it", *cluster.Spec.Paused)
	}
}

func TestUnpausingFailsUnlessBothControllersRanAfterIt(t *testing.T) {
	const notPausedByAController, noRunAfterTheUnpause = "did not leave Paused True on both", "both controllers did not run"
	cases := []struct {
		name          string
		before, after reported
		want          []string
	}{
		{"both paused, then run again", pausedByTheMove, runAgain, nil},
		{"Available recomputed at a later generation", pausedByTheMove, reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, unpausedGeneration + 3}, nil},
		{"neither controller ever reported the pause", beforeTheMove, beforeTheMove, []string{notPausedByAController, "the Paused of the Cluster", "the Paused of the LodiaControlPlane", "want True"}},
		{"the LodiaControlPlane never reported the pause", reported{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration - 1}, runAgain, []string{notPausedByAController, "the Paused of the LodiaControlPlane", "want True"}},
		{"the Cluster never reported the pause", reported{metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionTrue, pausedGeneration - 1}, runAgain, []string{notPausedByAController, "the Paused of the Cluster", "want True"}},
		{"the LodiaControlPlane has no Paused before the unpause", reported{metav1.ConditionTrue, notReported, metav1.ConditionTrue, pausedGeneration - 1}, runAgain, []string{notPausedByAController, "the Paused of the LodiaControlPlane is nil"}},
		{"the Cluster has no Paused before the unpause", reported{notReported, metav1.ConditionTrue, metav1.ConditionTrue, pausedGeneration - 1}, runAgain, []string{notPausedByAController, "the Paused of the Cluster is nil"}},
		{"neither controller ran after the unpause", pausedByTheMove, pausedByTheMove, []string{noRunAfterTheUnpause, "the Paused of the Cluster", "the Paused of the LodiaControlPlane", "want False", "the Available of the Cluster"}},
		{"the Cluster is still paused", pausedByTheMove, reported{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue, unpausedGeneration}, []string{noRunAfterTheUnpause, "the Paused of the Cluster", "want False"}},
		{"the LodiaControlPlane is still paused", pausedByTheMove, reported{metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionTrue, unpausedGeneration}, []string{noRunAfterTheUnpause, "the Paused of the LodiaControlPlane", "want False"}},
		{"the LodiaControlPlane has no Paused after the unpause", pausedByTheMove, reported{metav1.ConditionFalse, notReported, metav1.ConditionTrue, unpausedGeneration}, []string{noRunAfterTheUnpause, "the Paused of the LodiaControlPlane is nil"}},
		{"the Cluster has no Paused after the unpause", pausedByTheMove, reported{notReported, metav1.ConditionFalse, metav1.ConditionTrue, unpausedGeneration}, []string{noRunAfterTheUnpause, "the Paused of the Cluster is nil"}},
		{"Available is False at the generation of the unpause", pausedByTheMove, reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionFalse, unpausedGeneration}, []string{noRunAfterTheUnpause, "the Available of the Cluster"}},
		{"Available is missing", pausedByTheMove, reported{metav1.ConditionFalse, metav1.ConditionFalse, notReported, 0}, []string{noRunAfterTheUnpause, "the Available of the Cluster is nil"}},
		{"Available is the one of before the move", pausedByTheMove, reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration - 1}, []string{noRunAfterTheUnpause, "the Available of the Cluster", "True at generation 5 or later"}},
		{"Available is the one of the move's pause", pausedByTheMove, reported{metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration}, []string{noRunAfterTheUnpause, "the Available of the Cluster", "True at generation 5 or later"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bound := 100 * time.Millisecond
			if c.want == nil {
				bound = 5 * time.Second
			}
			err := requireUnpaused(t.Context(), newWorld(always(c.before), always(c.after)), unpausedKey, bound, 10*time.Millisecond)
			if c.want == nil && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if c.want != nil && err == nil {
				t.Fatalf("want an error with %q, got none", c.want)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("want an error with %q, got %v", want, err)
				}
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
