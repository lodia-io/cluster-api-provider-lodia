// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package clusterctlproxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr/funcr"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	logf "sigs.k8s.io/cluster-api/cmd/clusterctl/log"
)

// MoveUntilBlocked runs Move and ends it once clusterctl logs that it waits for
// the block-move annotation of the object of the given kind and name, which the
// wait itself would do for six minutes before it failed. It returns nil when
// Move then failed at its wait for the annotation:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L375-L377
// Otherwise it returns an error that holds what clusterctl logged, for a move
// that went through, one that failed for another reason, one that waits on
// another object and one whose context ended before the wait. The error of the
// context names no object, so the log is the only evidence.
//
// clusterctl's logger is global, and is replaced for the call.
func MoveUntilBlocked(ctx context.Context, c clusterctl.Client, options clusterctl.MoveOptions, kind, name string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var lines []string
	var blocked atomic.Bool
	previous := logf.Log
	defer logf.SetLogger(previous)
	logf.SetLogger(funcr.New(func(prefix, args string) {
		line := prefix + " " + args
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		if blockedOn(line, kind, options.Namespace, name) {
			blocked.Store(true)
			cancel()
		}
	}, funcr.Options{}))

	err := c.Move(ctx, options)
	if err != nil && blocked.Load() && strings.Contains(err.Error(), "error waiting for resources to be ready to move") {
		return nil
	}
	ended := "went through"
	if err != nil {
		ended = "ended with " + err.Error()
	}
	mu.Lock()
	defer mu.Unlock()
	return fmt.Errorf("the move %s, and not at its wait for the block-move annotation of %s %s/%s; clusterctl logged:\n%s", ended, kind, options.Namespace, name, strings.Join(lines, "\n"))
}

// blockedOn tells whether a line of funcr's rendering of clusterctl's log is its
// message that the move waits for the annotation of that object, with the
// object's kind and ObjectRef as clusterctl's log gives them:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L698-L704
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L728-L733
func blockedOn(line, kind, namespace, name string) bool {
	return strings.Contains(line, `"msg"="Move blocked by `+clusterctlv1.BlockMoveAnnotation+` annotation`) &&
		strings.Contains(line, ", Kind="+kind+`"`) &&
		strings.Contains(line, fmt.Sprintf(`"resource"={"name"=%q "namespace"=%q}`, name, namespace))
}
