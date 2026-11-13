// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	namespace         = "capi-join"
	lodiaNamespace    = "lodia-system"
	providerNamespace = "cluster-api-provider-lodia-system"
	clusterLabel      = "io.x-k8s.kind.cluster"
)

var (
	devCluster = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DevCluster"}
	devMachine = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DevMachine"}
)

// The Cluster is not named as the kind cluster: CAPD labels its containers
// with kind's own label, so kind counts them as its nodes.
func clusterName(cfg *envconf.Config) (string, error) {
	kind, err := kindCluster(cfg.KubeconfigFile())
	return kind + "-w", err
}

// A step's context has no deadline, so every call ends at a bound of its own.
func docker(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCallBound)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w\n%s", args[0], err, out)
	}
	return string(out), nil
}

func clusterContainers(ctx context.Context, cluster string) ([]string, error) {
	out, err := docker(ctx, "ps", "--all", "--quiet", "--filter", "label="+clusterLabel+"="+cluster)
	return strings.Fields(out), err
}

func kindCluster(kubeconfig string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(cfg.CurrentContext, "kind-"), nil
}

func shortenTheDockerCalls(t *testing.T, bound time.Duration) {
	t.Helper()
	was := dockerCallBound
	t.Cleanup(func() { dockerCallBound = was })
	dockerCallBound = bound
}

func dockerCalls() map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"ps": func(ctx context.Context) error {
			_, err := clusterContainers(ctx, "capl-e2e-w")
			return err
		},
		"exec": func(ctx context.Context) error {
			_, err := docker(ctx, "exec", "capl-e2e-control-plane", "cat", "/kind/manifests/default-cni.yaml")
			return err
		},
	}
}

func TestADockerCallThatHangsEndsAtItsBound(t *testing.T) {
	const bound = time.Second
	callers := map[string]func(t *testing.T) context.Context{
		"a caller without a deadline": func(*testing.T) context.Context { return context.Background() },
		"a caller with a later deadline": func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			t.Cleanup(cancel)
			return ctx
		},
	}
	for caller, newContext := range callers {
		for verb, call := range dockerCalls() {
			t.Run(caller+"/docker "+verb, func(t *testing.T) {
				shortenTheDockerCalls(t, bound)
				useDocker(t, dockerThatHangsOn(verb))
				ctx := newContext(t)
				start := time.Now()
				if err := returnsWithin(t, bound+bound/2, "the call", func() error { return call(ctx) }); err == nil {
					t.Fatal("the call says nothing about a docker that hung")
				}
				if took := time.Since(start); took < bound {
					t.Errorf("the call ended after %s, before its bound %s", took, bound)
				}
			})
		}
	}
}

func TestADockerCallThatHangsEndsWithItsCallersContext(t *testing.T) {
	const deadline = 300 * time.Millisecond
	for verb, call := range dockerCalls() {
		t.Run("docker "+verb, func(t *testing.T) {
			shortenTheDockerCalls(t, 10*time.Second)
			useDocker(t, dockerThatHangsOn(verb))
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			if err := returnsWithin(t, 3*time.Second, "the call", func() error { return call(ctx) }); err == nil {
				t.Fatal("the call says nothing about a docker that hung")
			}
		})
	}
}
