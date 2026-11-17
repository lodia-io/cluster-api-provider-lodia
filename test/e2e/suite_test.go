// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

var testenv env.Environment

func TestMain(m *testing.M) {
	path := os.Getenv("E2E_KUBECONFIG")
	if err := errors.Join(RequireNamedCluster(path), refuseHelmKubeEnv(os.Environ())); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, err := newEnvconf(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testenv = env.NewWithConfig(cfg)
	skip, err := skipReason()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if skip == "" {
		testenv.Setup(install)
	}
	os.Exit(testenv.Run(m))
}

func workDir(cfg *envconf.Config) string {
	return filepath.Dir(cfg.KubeconfigFile())
}

func dump(ctx context.Context, cfg *envconf.Config) {
	if err := dumpCluster(ctx, cfg, filepath.Join(workDir(cfg), "artifacts")); err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
	}
}

func install(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	bounded, cancel := within(ctx, suiteBounds.Install)
	defer cancel()
	for _, component := range []struct {
		name    string
		install func(context.Context, *envconf.Config) error
	}{
		{"cert-manager", installCertManager},
		{"Lodia", installLodia},
		{"Cluster API, the kubeadm bootstrap provider, Cluster API provider Docker and this provider, through clusterctl", installProviders},
	} {
		start := time.Now()
		if err := component.install(bounded, cfg); err != nil {
			dump(ctx, cfg)
			return ctx, fmt.Errorf("install %s: %w", component.name, errors.Join(err, context.Cause(bounded)))
		}
		fmt.Printf("installed %s after %s\n", component.name, time.Since(start).Round(time.Second))
	}
	return ctx, nil
}
