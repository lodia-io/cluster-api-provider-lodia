// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

// requireUnpaused undoes what a move that clusterctl began leaves, the Cluster
// paused, and requires that both controllers ran after it.
func requireUnpaused(ctx context.Context, c client.Client, key client.ObjectKey, bound, interval time.Duration) error {
	return pauseCycle(ctx, c, key, bound, interval, false, nil)
}

// A check that the end of ctx cuts short fails with the context's error, which
// says nothing about the step: the failure names the last check that ended
// before it.
func pollUntil(ctx context.Context, interval time.Duration, check func(context.Context) error) error {
	var last error
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		err := check(ctx)
		if ctx.Err() == nil {
			last = err
		}
		return err == nil, nil
	})
	if err != nil {
		return errors.Join(context.Cause(ctx), last)
	}
	return nil
}

var untilInterval = 2 * time.Second

func until(ctx context.Context, t testing.TB, timeout time.Duration, what string, check func(context.Context) error) {
	t.Helper()
	start := time.Now()
	bounded, cancel := endAfter(ctx, timeout)
	defer cancel()
	if err := pollUntil(bounded, untilInterval, check); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	t.Logf("%s, after %s", what, time.Since(start).Round(time.Second))
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
	return newWorldPausedAt(true, before, after)
}

func newWorldPausedAt(paused bool, before, after func(int) reported) *world {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: unpausedKey.Namespace, Name: unpausedKey.Name}}
	if paused {
		cluster.Spec.Paused = new(true)
	}
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: unpausedKey.Namespace, Name: unpausedKey.Name}}
	w := &world{}
	var unpaused bool
	w.Client = interceptor.NewClient(fake.NewClientBuilder().WithScheme(controller.Scheme()).WithObjects(cluster, lcp).Build(), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if patched, ok := obj.(*clusterv1.Cluster); ok && patched.Spec.Paused == nil {
				obj.SetGeneration(unpausedGeneration)
				unpaused = true
			}
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
	const notPausedByAController, noRunAfterTheUnpause = "Paused is not True on both", "both controllers did not run"
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
			w := newWorld(always(c.before), always(c.after))
			err := requireUnpaused(t.Context(), w, unpausedKey, bound, 10*time.Millisecond)
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
			cluster := &clusterv1.Cluster{}
			if err := w.Get(t.Context(), unpausedKey, cluster); err != nil {
				t.Fatal(err)
			}
			if left := err != nil && strings.Contains(err.Error(), notPausedByAController); (cluster.Spec.Paused != nil) != left {
				t.Errorf("spec.paused is %v after the call, want it left as the move left it only when neither controller reported the pause", cluster.Spec.Paused)
			}
		})
	}
}

func TestUnpausingTakesOneBoundForBothWaits(t *testing.T) {
	const (
		bound      = 800 * time.Millisecond
		reportedAt = 3 * bound / 4
	)
	start := time.Now()
	w := newWorld(func(int) reported {
		if time.Since(start) < reportedAt {
			return beforeTheMove
		}
		return pausedByTheMove
	}, always(pausedByTheMove))
	err := requireUnpaused(t.Context(), w, unpausedKey, bound, 10*time.Millisecond)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "both controllers did not run") {
		t.Fatalf("want the failure of the second wait, got %v", err)
	}
	if limit := bound + bound/4; took > limit {
		t.Errorf("the waits took %s, more than %s: the second one has a bound of its own, and not what the first left of the %s that both share", took, limit, bound)
	}
}

func TestUnpausingFailsWithoutACluster(t *testing.T) {
	err := requireUnpaused(t.Context(), fake.NewClientBuilder().WithScheme(controller.Scheme()).Build(), unpausedKey, 100*time.Millisecond, 50*time.Millisecond)
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("want a NotFound, got %v", err)
	}
}

func shortenTheUntil(t *testing.T, interval time.Duration) {
	t.Helper()
	was := untilInterval
	t.Cleanup(func() { untilInterval = was })
	untilInterval = interval
}

func throttled(ctx context.Context, limiter flowcontrol.RateLimiter, deadlined *atomic.Int32) error {
	if _, ok := ctx.Deadline(); ok {
		deadlined.Add(1)
	}
	if err := limiter.Wait(ctx); err != nil {
		return fmt.Errorf("client rate limiter Wait returned an error: %w", err)
	}
	return nil
}

type failing struct {
	testing.TB
	message string
}

func (f *failing) Helper() {}

func (f *failing) Logf(string, ...any) {}

func (f *failing) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func failureOf(t *testing.T, call func(testing.TB)) string {
	t.Helper()
	f := &failing{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(f)
	}()
	<-done
	return f.message
}

func requireEndedAtItsBound(t *testing.T, took, bound time.Duration) {
	t.Helper()
	if took < bound {
		t.Errorf("the call ended after %s, before its bound %s", took, bound)
	}
	if limit := bound + bound/2; took > limit {
		t.Errorf("the call took %s, more than %s: it has to end at its bound %s", took, limit, bound)
	}
}

func requireNamed(t *testing.T, err error, deadlined *atomic.Int32, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a failure with %q, got none", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("want the failure to say %q, got %v", w, err)
		}
	}
	if strings.Contains(err.Error(), "rate limiter") {
		t.Errorf("the failure blames the limiter: %v", err)
	}
	if n := deadlined.Load(); n > 0 {
		t.Errorf("%d reads ran under a deadline, which makes the limiter refuse a read at once with an error that is no context's", n)
	}
}

func TestUntilFailsWithTheCauseOfItsEndAndTheReasonOfTheLastCheckWhereTheClientsLimiterWouldHoldARead(t *testing.T) {
	const bound = 300 * time.Millisecond
	shortenTheUntil(t, 20*time.Millisecond)
	cases := []struct {
		name     string
		passesAt int
		failure  []string
	}{
		{"a check that never passes", 0, []string{"the step: context deadline exceeded", "the real reason"}},
		{"a check that passes at its first read", 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var deadlined atomic.Int32
			limiter := flowcontrol.NewTokenBucketRateLimiter(0.001, 1)
			var reads int
			start := time.Now()
			message := failureOf(t, func(f testing.TB) {
				until(t.Context(), f, bound, "the step", func(ctx context.Context) error {
					reads++
					if err := throttled(ctx, limiter, &deadlined); err != nil {
						return err
					}
					if reads == c.passesAt {
						return nil
					}
					return errors.New("the real reason")
				})
			})
			if c.failure == nil {
				if message != "" {
					t.Fatalf("want no failure, got %s", message)
				}
				return
			}
			requireEndedAtItsBound(t, time.Since(start), bound)
			requireNamed(t, errors.New(message), &deadlined, c.failure...)
		})
	}
}

type throttledGets struct {
	client.Client
	limiter   flowcontrol.RateLimiter
	deadlined *atomic.Int32
}

func (g throttledGets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := throttled(ctx, g.limiter, g.deadlined); err != nil {
		return err
	}
	return g.Client.Get(ctx, key, obj, opts...)
}

func TestAPauseNamesWhatTheControllersDidNotReportWhereTheClientsLimiterWouldHoldAReadPastTheBound(t *testing.T) {
	lcpNeverReports := reported{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue, pausedGeneration - 1}
	unpausing := func(c client.Client) error {
		return requireUnpaused(t.Context(), c, unpausedKey, 200*time.Millisecond, 10*time.Millisecond)
	}
	pausing := func(c client.Client) error {
		return whilePaused(t.Context(), c, unpausedKey, 200*time.Millisecond, 10*time.Millisecond, nil)
	}
	cases := []struct {
		name   string
		call   func(client.Client) error
		world  func() *world
		passes int
		want   []string
	}{
		{"the first wait of an unpause", unpausing, func() *world { return newWorld(always(lcpNeverReports), always(runAgain)) }, 2, []string{"Paused is not True on both", "the Paused of the LodiaControlPlane"}},
		{"the second wait of an unpause", unpausing, func() *world { return newWorld(always(pausedByTheMove), always(pausedByTheMove)) }, 4, []string{"both controllers did not run", "the Paused of the Cluster"}},
		{"the first wait of a pause", pausing, func() *world { return newWorldPausedAt(false, always(lcpNeverReports), always(runAgain)) }, 2, []string{"Paused is not True on both", "the Paused of the LodiaControlPlane"}},
		{"the second wait of a pause", pausing, func() *world { return newWorldPausedAt(false, always(pausedByTheMove), always(pausedByTheMove)) }, 4, []string{"both controllers did not run", "the Paused of the Cluster"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var deadlined atomic.Int32
			err := c.call(throttledGets{c.world(), flowcontrol.NewTokenBucketRateLimiter(0.001, c.passes), &deadlined})
			requireNamed(t, err, &deadlined, c.want...)
		})
	}
}
