// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

// Cluster API waits until four reads 15 seconds apart agree, and then holds
// them for the window with a read every 5 seconds:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/framework/resourceversion_helpers.go#L44-L76
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/e2e/config/docker.yaml#L272-L273
const (
	settleInterval = 15 * time.Second
	settleReads    = 4
	holdInterval   = 5 * time.Second
	holdWindow     = 2 * time.Minute
)

type stamp struct{ what, value string }

type snapshot map[string]stamp

// snapshotOf stamps the graph by resourceVersion but for two objects.
// etcd-druid rewrites the Etcd's status every 15 seconds with a new
// lastUpdateTime, which Lodia's own watch filters out for the same reason.
// Lodia writes the ControlPlane's status itself, so the provider's own entry
// of managedFields stands for it: an apply that changes something moves the
// entry, and Lodia's writes do not.
// https://github.com/gardener/etcd-druid/blob/v0.39.0/internal/controller/etcd/reconciler.go#L147
// https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/internal/controller/datastore/etcd.go#L397-L407
func snapshotOf(ctx context.Context, c client.Reader, namespace string) (snapshot, error) {
	graph, err := listGraph(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	stamps := snapshot{}
	for _, obj := range graph {
		switch obj.GroupVersionKind() {
		case etcdKind:
		case controlPlaneKind:
			var entries []string
			for _, entry := range obj.GetManagedFields() {
				if entry.Manager != controller.FieldManager {
					continue
				}
				raw, err := json.Marshal(entry)
				if err != nil {
					return nil, err
				}
				entries = append(entries, string(raw))
			}
			if len(entries) == 0 {
				return nil, fmt.Errorf("%s has no entry of %s in managedFields: nothing shows a loop of the provider's", describe(obj), controller.FieldManager)
			}
			slices.Sort(entries)
			stamps[describe(obj)] = stamp{"managedFields entry of " + controller.FieldManager, strings.Join(entries, "\n")}
		default:
			stamps[describe(obj)] = stamp{"resourceVersion", obj.GetResourceVersion()}
		}
	}
	return stamps, nil
}

func (s snapshot) changesTo(next snapshot) []string {
	var changes []string
	for _, name := range slices.Sorted(maps.Keys(s)) {
		switch was, now := s[name], next[name]; {
		case now == stamp{}:
			changes = append(changes, name+" is gone")
		case now != was && now.what == "resourceVersion":
			changes = append(changes, fmt.Sprintf("%s: the resourceVersion went from %s to %s", name, was.value, now.value))
		case now != was:
			changes = append(changes, fmt.Sprintf("%s: the %s changed", name, now.what))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(next)) {
		if _, ok := s[name]; !ok {
			changes = append(changes, name+" is new")
		}
	}
	return changes
}

// endAfter ends the context after d as context.WithTimeout does, but by
// cancelling it. A request that starts under a deadline is refused by client-go's
// rate limiter at once, with an error that is no context's, when its token is
// due after the deadline or when the deadline has just passed and the context's
// timer has not run, which a tick at the end of a window brings about:
// https://github.com/golang/time/blob/v0.15.0/rate/rate.go#L268-L277
// https://github.com/kubernetes/client-go/blob/v0.37.2/rest/request.go#L672-L678
func endAfter(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(d, func() { cancel(fmt.Errorf("%w after %s", context.DeadlineExceeded, d)) })
	return ctx, func() {
		timer.Stop()
		cancel(context.Canceled)
	}
}

// endedBy gives a poll's error the cause that endAfter gave its context, since
// the poll reports only context.Canceled.
func endedBy(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && wait.Interrupted(err) {
		return cause
	}
	return err
}

// cutShort gives the error of a call that the end of bounded cut short the
// cause that endAfter gave it, since the call reports only context.Canceled.
func cutShort(bounded context.Context, err error) error {
	cause := context.Cause(bounded)
	if err == nil || cause == nil {
		return err
	}
	return errors.Join(err, cause)
}

// settle waits until settleReads reads in a row agree with the one before,
// which the first read has none of.
func settle(ctx context.Context, c client.Reader, namespace string, bound, interval time.Duration) (snapshot, error) {
	bounded, cancel := endAfter(ctx, bound)
	defer cancel()
	var previous snapshot
	var agreed int
	var last error
	err := wait.PollUntilContextCancel(bounded, interval, true, func(ctx context.Context) (bool, error) {
		current, err := snapshotOf(ctx, c, namespace)
		if err != nil {
			previous, agreed = nil, 0
			if ctx.Err() == nil {
				last = err
			}
			return false, nil
		}
		switch changes := previous.changesTo(current); {
		case previous == nil:
			agreed, last = 0, errors.New("the first read has none before it to agree with")
		case len(changes) > 0:
			agreed, last = 0, errors.New(strings.Join(changes, "; "))
		default:
			agreed++
		}
		previous = current
		return agreed >= settleReads, nil
	})
	if err != nil {
		return nil, fmt.Errorf("after %s, %d reads in a row have not agreed: %w", bound, settleReads, errors.Join(context.Cause(bounded), last))
	}
	return previous, nil
}

func hold(ctx context.Context, c client.Reader, namespace string, window, interval time.Duration, baseline snapshot) error {
	windowed, cancel := endAfter(ctx, window)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-windowed.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		current, err := snapshotOf(windowed, c, namespace)
		if windowed.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if changes := baseline.changesTo(current); len(changes) > 0 {
			return fmt.Errorf("within the window of %s: %s", window, strings.Join(changes, "; "))
		}
	}
}

// The reads of the graph are counted by the lists of CRDs, of which each makes
// one.
type scriptedReads struct {
	client.Reader
	n         atomic.Int32
	lists     atomic.Int32
	deadlined atomic.Int32
	change    func(read int, kind string, obj *unstructured.Unstructured)
	hangFrom  int
	failFrom  int
	limiter   flowcontrol.RateLimiter
}

func (r *scriptedReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists.Add(1)
	if _, ok := ctx.Deadline(); ok {
		r.deadlined.Add(1)
	}
	if r.limiter != nil {
		if err := r.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("client rate limiter Wait returned an error: %w", err)
		}
	}
	if r.failFrom > 0 && int(r.n.Load()) >= r.failFrom {
		return errors.New("the API server is gone")
	}
	if r.hangFrom > 0 && int(r.n.Load()) >= r.hangFrom {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	items, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return nil
	}
	kind := strings.TrimSuffix(items.GetKind(), "List")
	if kind == "CustomResourceDefinition" {
		r.n.Add(1)
	}
	if r.change != nil {
		for i := range items.Items {
			r.change(int(r.n.Load()), kind, &items.Items[i])
		}
	}
	return nil
}

func scripted(change func(read int, kind string, obj *unstructured.Unstructured)) *scriptedReads {
	return &scriptedReads{Reader: newQuickStartNamespace().client(), change: change}
}

func limitedToSnapshots(t *testing.T, r *scriptedReads, snapshots int) {
	t.Helper()
	counted := scripted(nil)
	if _, err := snapshotOf(t.Context(), counted, quickStartKey.Namespace); err != nil {
		t.Fatal(err)
	}
	r.limiter = flowcontrol.NewTokenBucketRateLimiter(0.001, snapshots*int(counted.lists.Load()))
}

func providersEntry(obj *unstructured.Unstructured, change func(*metav1.ManagedFieldsEntry)) {
	entries := obj.GetManagedFields()
	for i := range entries {
		if entries[i].Manager == controller.FieldManager {
			change(&entries[i])
		}
	}
	obj.SetManagedFields(entries)
}

func TestTheStampsHoldWhatTheProviderWritesAndNotWhatLodiaAndEtcdDruidDo(t *testing.T) {
	cases := []struct {
		name    string
		change  func(kind string, obj *unstructured.Unstructured)
		changes string
		failure string
	}{
		{"nothing changes", func(string, *unstructured.Unstructured) {}, "", ""},
		{"etcd-druid rewrites the Etcd's status", func(kind string, obj *unstructured.Unstructured) {
			if kind == "Etcd" {
				obj.SetResourceVersion("rewritten")
			}
		}, "", ""},
		{"Lodia writes the ControlPlane's status", func(kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" {
				obj.SetResourceVersion("written")
			}
		}, "", ""},
		{"Lodia's own entry of managedFields moves", func(kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" {
				entries := obj.GetManagedFields()
				entries[1].Time = &metav1.Time{Time: time.Unix(2000, 0)}
				obj.SetManagedFields(entries)
			}
		}, "", ""},
		{"the provider's apply changes the ControlPlane", func(kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" {
				obj.SetResourceVersion("applied")
				providersEntry(obj, func(e *metav1.ManagedFieldsEntry) { e.Time = &metav1.Time{Time: time.Unix(2000, 0)} })
			}
		}, "ControlPlane w: the managedFields entry of cluster-api-provider-lodia changed", ""},
		{"the provider takes another field of the ControlPlane", func(kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" {
				providersEntry(obj, func(e *metav1.ManagedFieldsEntry) {
					e.FieldsV1 = metav1.NewFieldsV1(`{"f:spec":{"f:other":{}}}`)
				})
			}
		}, "ControlPlane w: the managedFields entry of cluster-api-provider-lodia changed", ""},
		{"a Machine is written", func(kind string, obj *unstructured.Unstructured) {
			if kind == "Machine" {
				obj.SetResourceVersion("written")
			}
		}, "Machine w-md-0-abc-1: the resourceVersion went from 999 to written", ""},
		{"a Secret is written", func(kind string, obj *unstructured.Unstructured) {
			if kind == "Secret" && obj.GetName() == "w-kubeconfig" {
				obj.SetResourceVersion("written")
			}
		}, "Secret w-kubeconfig: the resourceVersion", ""},
		{"the LodiaControlPlane is written", func(kind string, obj *unstructured.Unstructured) {
			if kind == "LodiaControlPlane" {
				obj.SetResourceVersion("written")
			}
		}, "LodiaControlPlane w: the resourceVersion", ""},
		{"the Cluster is written", func(kind string, obj *unstructured.Unstructured) {
			if kind == "Cluster" {
				obj.SetResourceVersion("written")
			}
		}, "Cluster w: the resourceVersion", ""},
		{"the ControlPlane has no entry of the provider's", func(kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" {
				obj.SetManagedFields(nil)
			}
		}, "", "has no entry of cluster-api-provider-lodia in managedFields"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := snapshotOf(t.Context(), scripted(nil), quickStartKey.Namespace)
			requireNoError(t, err)
			after, err := snapshotOf(t.Context(), scripted(func(_ int, kind string, obj *unstructured.Unstructured) { c.change(kind, obj) }), quickStartKey.Namespace)
			requireFailure(t, err, c.failure)
			if err != nil {
				return
			}
			changes := strings.Join(before.changesTo(after), "; ")
			if (c.changes == "") != (changes == "") || !strings.Contains(changes, c.changes) {
				t.Fatalf("the stamps changed by %q, want %q", changes, c.changes)
			}
		})
	}
}

func TestTheStampsSeeAnObjectThatComesOrGoes(t *testing.T) {
	before, err := snapshotOf(t.Context(), scripted(nil), quickStartKey.Namespace)
	requireNoError(t, err)
	q := newQuickStartNamespace()
	q.objects = slices.DeleteFunc(q.objects, func(obj *unstructured.Unstructured) bool {
		return obj.GetName() == "w-md-0-abc-1" && obj.GetKind() == "Secret"
	})
	q.add(object(clusterAPIKind("Machine"), "w-md-0-abc-2"))
	after, err := snapshotOf(t.Context(), q.client(), quickStartKey.Namespace)
	requireNoError(t, err)
	got := strings.Join(before.changesTo(after), "; ")
	for _, want := range []string{"Secret w-md-0-abc-1 is gone", "Machine w-md-0-abc-2 is new"} {
		if !strings.Contains(got, want) {
			t.Errorf("the changes %q lack %q", got, want)
		}
	}
}

func TestSettleReturnsAfterFourReadsInARowThatAgree(t *testing.T) {
	written := func(until int) func(int, string, *unstructured.Unstructured) {
		return func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "Machine" {
				obj.SetResourceVersion(fmt.Sprint(min(read, until)))
			}
		}
	}
	cases := []struct {
		name      string
		change    func(int, string, *unstructured.Unstructured)
		wantReads int32
		failure   string
	}{
		{"nothing changes: the first read has none to agree with, and four agree", nil, 5, ""},
		{"a Machine written by the third read: four reads agree after it", written(3), 7, ""},
		{"three agree, then a write, then four agree", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "Machine" {
				obj.SetResourceVersion(fmt.Sprint(map[bool]int{true: 1, false: 2}[read <= 4]))
			}
		}, 9, ""},
		{"a write on every read", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "Machine" {
				obj.SetResourceVersion(fmt.Sprint(read))
			}
		}, 0, "have not agreed"},
		{"a loop of the provider's on the ControlPlane", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" {
				providersEntry(obj, func(e *metav1.ManagedFieldsEntry) { e.Time = &metav1.Time{Time: time.Unix(int64(read), 0)} })
			}
		}, 0, "the managedFields entry of cluster-api-provider-lodia changed"},
		{"a ControlPlane that Lodia writes on every read", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" || kind == "Etcd" {
				obj.SetResourceVersion(fmt.Sprint(read))
			}
		}, 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := scripted(c.change)
			stable, err := settle(t.Context(), r, quickStartKey.Namespace, 500*time.Millisecond, 10*time.Millisecond)
			requireFailure(t, err, c.failure)
			if err != nil {
				return
			}
			if got := r.n.Load(); got != c.wantReads {
				t.Errorf("settled after %d reads, want %d", got, c.wantReads)
			}
			if len(stable) == 0 {
				t.Error("settle returned no snapshot to hold the window against")
			}
		})
	}
}

func TestHoldFailsAtTheFirstChangeAndOtherwiseTakesTheWholeWindow(t *testing.T) {
	const interval, window = 250 * time.Millisecond, 5*250*time.Millisecond + 125*time.Millisecond
	baseline, err := snapshotOf(t.Context(), scripted(nil), quickStartKey.Namespace)
	requireNoError(t, err)
	cases := []struct {
		name     string
		change   func(read int, kind string, obj *unstructured.Unstructured)
		hangFrom int
		failure  string
	}{
		{"nothing changes", nil, 0, ""},
		{"Lodia and etcd-druid write on every read", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "Etcd" || kind == "ControlPlane" {
				obj.SetResourceVersion(fmt.Sprint(read))
			}
		}, 0, ""},
		{"a read that the end of the window cuts short", nil, 3, ""},
		{"a Machine is written on the third read", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "Machine" && read >= 3 {
				obj.SetResourceVersion("written")
			}
		}, 0, "Machine w-md-0-abc-1: the resourceVersion"},
		{"the provider's apply changes the ControlPlane on the third read", func(read int, kind string, obj *unstructured.Unstructured) {
			if kind == "ControlPlane" && read >= 3 {
				providersEntry(obj, func(e *metav1.ManagedFieldsEntry) { e.Time = &metav1.Time{Time: time.Unix(int64(read), 0)} })
			}
		}, 0, "the managedFields entry of cluster-api-provider-lodia changed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := scripted(c.change)
			r.hangFrom = c.hangFrom
			start := time.Now()
			err := hold(t.Context(), r, quickStartKey.Namespace, window, interval, baseline)
			requireFailure(t, err, c.failure)
			switch took := time.Since(start); {
			case c.failure == "" && took < window:
				t.Errorf("the window ended after %s, before its %s", took, window)
			case c.failure != "" && took >= window:
				t.Errorf("the change was found after %s, not at the first read that saw it", took)
			}
		})
	}
}

func TestHoldEndsQuietlyWhereTheClientsLimiterWouldHoldAReadPastTheWindowAndFailsOnAnyOtherError(t *testing.T) {
	const interval, window = 100 * time.Millisecond, 5*100*time.Millisecond + 50*time.Millisecond
	baseline, err := snapshotOf(t.Context(), scripted(nil), quickStartKey.Namespace)
	requireNoError(t, err)
	written := func(read int, kind string, obj *unstructured.Unstructured) {
		if kind == "Machine" && read >= 2 {
			obj.SetResourceVersion("written")
		}
	}
	cases := []struct {
		name     string
		change   func(read int, kind string, obj *unstructured.Unstructured)
		snapshot int
		failFrom int
		failure  string
	}{
		{"the token of the third snapshot is due after the window", nil, 2, 0, ""},
		{"a Machine is written on the second read, ahead of a limiter that holds a later one", written, 3, 0, "Machine w-md-0-abc-1: the resourceVersion"},
		{"a read fails on the third, ahead of a limiter that holds a later one", nil, 4, 2, "the API server is gone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := scripted(c.change)
			r.failFrom = c.failFrom
			if c.snapshot > 0 {
				limitedToSnapshots(t, r, c.snapshot)
			}
			start := time.Now()
			err := hold(t.Context(), r, quickStartKey.Namespace, window, interval, baseline)
			requireFailure(t, err, c.failure)
			if took := time.Since(start); c.failure == "" && took < window {
				t.Errorf("the window ended after %s, before its %s", took, window)
			}
			if n := r.deadlined.Load(); n > 0 {
				t.Errorf("%d reads ran under a deadline, which makes the limiter refuse a read at once with an error that is no context's", n)
			}
		})
	}
}

func TestSettleNamesWhyTheReadsDisagreeWhereTheClientsLimiterWouldHoldAReadPastItsBound(t *testing.T) {
	const interval, bound = 50 * time.Millisecond, 400 * time.Millisecond
	written := func(read int, kind string, obj *unstructured.Unstructured) {
		if kind == "Machine" {
			obj.SetResourceVersion(fmt.Sprint(read))
		}
	}
	cases := []struct {
		name     string
		change   func(read int, kind string, obj *unstructured.Unstructured)
		snapshot int
		failFrom int
		failure  string
	}{
		{"writes on every read, and a limiter that holds the third", written, 2, 0, "the resourceVersion went from 1 to 2"},
		{"a read fails", nil, 0, 1, "the API server is gone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := scripted(c.change)
			r.failFrom = c.failFrom
			if c.snapshot > 0 {
				limitedToSnapshots(t, r, c.snapshot)
			}
			_, err := settle(t.Context(), r, quickStartKey.Namespace, bound, interval)
			if err == nil || !strings.Contains(err.Error(), "have not agreed") || !strings.Contains(err.Error(), c.failure) {
				t.Errorf("want a failure to agree for the reason %q, got %v", c.failure, err)
			}
			if err != nil && strings.Contains(err.Error(), "rate limiter") {
				t.Errorf("the failure blames the limiter: %v", err)
			}
			if n := r.deadlined.Load(); n > 0 {
				t.Errorf("%d reads ran under a deadline, which makes the limiter refuse a read at once with an error that is no context's", n)
			}
		})
	}
}

func TestTheStabilityChecksReadAsOftenAndAsLongAsClusterAPIsDo(t *testing.T) {
	if span := settleReads * settleInterval; span < time.Minute {
		t.Errorf("%d reads %s apart agree over %s, less than the minute that Cluster API's four reads 15 seconds apart do", settleReads, settleInterval, span)
	}
	if holdInterval > 5*time.Second {
		t.Errorf("the window is read every %s, less often than Cluster API's 5 seconds", holdInterval)
	}
	if bounds.Stable < holdWindow {
		t.Errorf("the window of step 23 is %s, shorter than Cluster API's %s", bounds.Stable, holdWindow)
	}
}

func TestAnErrorOfACallThatItsBoundCutShortNamesTheBoundAndTheCauseOfTheEnd(t *testing.T) {
	ended, cancel := endAfter(t.Context(), time.Millisecond)
	defer cancel()
	<-ended.Done()
	running, stop := endAfter(t.Context(), time.Minute)
	defer stop()
	refused := errors.New("forbidden")

	err := cutShort(ended, fmt.Errorf("patch: %w", context.Canceled))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "patch") || !strings.Contains(err.Error(), "context deadline exceeded after 1ms") {
		t.Errorf("a call cut short fails with %v, want its own error and the cause of the end with its bound", err)
	}
	if err := cutShort(running, refused); !errors.Is(err, refused) || err.Error() != refused.Error() {
		t.Errorf("a call that failed before its bound ended fails with %v, want its own error as it is", err)
	}
	if err := cutShort(ended, nil); err != nil {
		t.Errorf("a call that did not fail fails with %v", err)
	}
}
