// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const clusterLabel = "io.x-k8s.kind.cluster"

var (
	testenv env.Environment
	root    = filepath.Join("..", "..")
)

func TestMain(m *testing.M) {
	path := os.Getenv("E2E_KUBECONFIG")
	if err := RequireNamedCluster(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testenv = env.NewWithConfig(envconf.NewWithKubeConfig(path))
	testenv.Setup(install)
	os.Exit(testenv.Run(m))
}

func install(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	for _, component := range []struct {
		name    string
		install func(context.Context, *envconf.Config) error
	}{
		{"cert-manager", installCertManager},
		{"Cluster API and the kubeadm bootstrap provider", installClusterAPI},
		{"Cluster API provider Docker", installCAPD},
		{"Lodia", installLodia},
		{"the provider", installProvider},
	} {
		start := time.Now()
		if err := component.install(bounded, cfg); err != nil {
			dump(ctx, cfg)
			return ctx, fmt.Errorf("install %s: %w", component.name, err)
		}
		fmt.Printf("installed %s after %s\n", component.name, time.Since(start).Round(time.Second))
	}
	return ctx, nil
}

func workDir(cfg *envconf.Config) string {
	return filepath.Dir(cfg.KubeconfigFile())
}

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

func kindCluster(kubeconfig string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(cfg.CurrentContext, "kind-"), nil
}
