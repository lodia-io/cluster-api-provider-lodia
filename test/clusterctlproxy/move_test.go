// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package clusterctlproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	logf "sigs.k8s.io/cluster-api/cmd/clusterctl/log"
)

const clusterctlsLine = ` "level"=0 "msg"="Move blocked by clusterctl.cluster.x-k8s.io/block-move annotation, waiting for it to be removed" "apiVersion"="controlplane.lodia.io/v1alpha1, Kind=LodiaControlPlane" "resource"={"name"="moved-cp" "namespace"="test-tm2bt"}`

func TestTheBlockMessageIsRecognizedByItsObject(t *testing.T) {
	cases := []struct {
		name                   string
		line, kind, ns, object string
		want                   bool
	}{
		{"the line clusterctl logs at v1.15.0-beta.0", clusterctlsLine, "LodiaControlPlane", "test-tm2bt", "moved-cp", true},
		{"another kind", clusterctlsLine, "KubeadmControlPlane", "test-tm2bt", "moved-cp", false},
		{"another name", clusterctlsLine, "LodiaControlPlane", "test-tm2bt", "other-cp", false},
		{"another namespace", clusterctlsLine, "LodiaControlPlane", "elsewhere", "moved-cp", false},
		{"a kind that ends with the wanted one", clusterctlsLine, "ControlPlane", "test-tm2bt", "moved-cp", false},
		{"another message about the object", strings.Replace(clusterctlsLine, "Move blocked by", "Resource is not blocked by", 1), "LodiaControlPlane", "test-tm2bt", "moved-cp", false},
		{"another annotation", strings.Replace(clusterctlsLine, "block-move", "move", 1), "LodiaControlPlane", "test-tm2bt", "moved-cp", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := blockedOn(c.line, c.kind, c.ns, c.object); got != c.want {
				t.Errorf("blockedOn is %t, want %t", got, c.want)
			}
		})
	}
}

type moves struct {
	clusterctl.Client
	run func(context.Context) error
}

func (m moves) Move(ctx context.Context, _ clusterctl.MoveOptions) error { return m.run(ctx) }

func logBlocked(kind, name string) {
	logf.Log.WithValues(
		"apiVersion", schema.GroupVersionKind{Group: "controlplane.lodia.io", Version: "v1alpha1", Kind: kind},
		"resource", klog.ObjectRef{Name: name, Namespace: "ns"},
	).Info("Move blocked by clusterctl.cluster.x-k8s.io/block-move annotation, waiting for it to be removed")
}

func failsAtTheWait(ctx context.Context) error {
	<-ctx.Done()
	return fmt.Errorf("error waiting for resources to be ready to move: action failed after 1 attempts: %w", ctx.Err())
}

func TestMoveUntilBlockedEndsAtTheWaitOnTheObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := moves{run: func(ctx context.Context) error {
		logf.Log.Info("Performing move...")
		logBlocked("LodiaControlPlane", "cp")
		return failsAtTheWait(ctx)
	}}

	start := time.Now()
	if err := MoveUntilBlocked(ctx, c, clusterctl.MoveOptions{Namespace: "ns"}, "LodiaControlPlane", "cp"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the call took %s, and did not end the wait that it saw", took)
	}
}

func TestMoveUntilBlockedRefusesWhatIsNotTheWaitOnTheObject(t *testing.T) {
	cases := map[string]struct {
		run  func(context.Context) error
		want string
	}{
		"a move that goes through": {
			run:  func(context.Context) error { return nil },
			want: "the move went through",
		},
		"a move that fails for another reason": {
			run: func(context.Context) error {
				logf.Log.Info("Performing move...")
				return errors.New("failed to get object graph")
			},
			want: "failed to get object graph",
		},
		"a wait on another object": {
			run: func(ctx context.Context) error {
				logBlocked("LodiaControlPlane", "another")
				return failsAtTheWait(ctx)
			},
			want: "the move ended with error waiting",
		},
		"a wait on an object of another kind": {
			run: func(ctx context.Context) error {
				logBlocked("KubeadmControlPlane", "cp")
				return failsAtTheWait(ctx)
			},
			want: "the move ended with error waiting",
		},
		"a context that ends before the wait": {
			run: func(ctx context.Context) error {
				logf.Log.Info("Discovering Cluster API objects")
				return failsAtTheWait(ctx)
			},
			want: "Discovering Cluster API objects",
		},
		"a failure after the block that is not the wait": {
			run: func(context.Context) error {
				logBlocked("LodiaControlPlane", "cp")
				return errors.New("error pausing ClusterClasses")
			},
			want: "error pausing ClusterClasses",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			err := MoveUntilBlocked(ctx, moves{run: c.run}, clusterctl.MoveOptions{Namespace: "ns"}, "LodiaControlPlane", "cp")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
