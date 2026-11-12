// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
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

func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w\n%s", args[0], err, out)
	}
	return string(out), nil
}

func stepDocker(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, stepDockerBound)
	defer cancel()
	return docker(ctx, args...)
}

func clusterContainers(ctx context.Context, cluster string) ([]string, error) {
	out, err := stepDocker(ctx, "ps", "--all", "--quiet", "--filter", "label="+clusterLabel+"="+cluster)
	return strings.Fields(out), err
}

func kindCluster(kubeconfig string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(cfg.CurrentContext, "kind-"), nil
}

func shortenTheStepsDocker(t *testing.T, bound time.Duration) {
	t.Helper()
	was := stepDockerBound
	t.Cleanup(func() { stepDockerBound = was })
	stepDockerBound = bound
}

func stepDockerCalls() map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"ps": func(ctx context.Context) error {
			_, err := clusterContainers(ctx, "capl-e2e-w")
			return err
		},
		"exec": func(ctx context.Context) error {
			_, err := stepDocker(ctx, "exec", "capl-e2e-control-plane", "cat", "/kind/manifests/default-cni.yaml")
			return err
		},
	}
}

func TestADockerCallOfAStepThatHangsEndsAtItsBound(t *testing.T) {
	const bound = time.Second
	for verb, call := range stepDockerCalls() {
		t.Run("docker "+verb, func(t *testing.T) {
			shortenTheStepsDocker(t, bound)
			useDocker(t, dockerThatHangsOn(verb))
			start := time.Now()
			if err := returnsWithin(t, bound+bound/2, "the call", func() error { return call(context.Background()) }); err == nil {
				t.Fatal("the call says nothing about a docker that hung")
			}
			if took := time.Since(start); took < bound {
				t.Errorf("the call ended after %s, before its bound %s", took, bound)
			}
		})
	}
}

func TestADockerCallOfAStepThatHangsEndsWithItsCallersContext(t *testing.T) {
	const deadline = 300 * time.Millisecond
	for verb, call := range stepDockerCalls() {
		t.Run("docker "+verb, func(t *testing.T) {
			shortenTheStepsDocker(t, 10*time.Second)
			useDocker(t, dockerThatHangsOn(verb))
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			if err := returnsWithin(t, 3*time.Second, "the call", func() error { return call(ctx) }); err == nil {
				t.Fatal("the call says nothing about a docker that hung")
			}
		})
	}
}

func TestTheStepsCallDockerOnlyThroughItsBound(t *testing.T) {
	bounded := []string{"stepDocker", "dumpWorkers"}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || slices.Contains(bounded, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "docker" {
					t.Errorf("%s: %s uses docker, which has no bound of its own: call stepDocker", file, fn.Name.Name)
				}
				return true
			})
		}
	}
}
