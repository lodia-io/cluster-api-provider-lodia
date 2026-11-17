// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e && upgrade

package e2e

import (
	"context"
	"fmt"
	"path/filepath"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"go.lodia.io/cluster-api-provider-lodia/test/pin"
)

func skipReason() (string, error) {
	release, err := pin.Previous()
	if err != nil || release.Upgrades() {
		return "", err
	}
	return fmt.Sprintf("%s sets upgrade: false for %s: nothing is installed and the upgrade test is skipped", pin.File, release.Version), nil
}

func lodiaProviders(ctx context.Context, cfg *envconf.Config) ([]clusterctlProvider, string, error) {
	tree, err := lodiaProvider(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	previous, err := previousProvider(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	return []clusterctlProvider{previous, tree}, "lodia:" + previous.version, nil
}

func previousProvider(ctx context.Context, cfg *envconf.Config) (clusterctlProvider, error) {
	release, err := pin.Previous()
	if err != nil {
		return clusterctlProvider{}, err
	}
	dir := filepath.Join(workDir(cfg), release.Version)
	components, err := pin.Fetch(ctx, dir, release.ComponentsURL(), release.Components)
	if err != nil {
		return clusterctlProvider{}, err
	}
	metadata, err := pin.Fetch(ctx, dir, release.MetadataURL(), release.Metadata)
	if err != nil {
		return clusterctlProvider{}, err
	}
	pinned, err := requireDigests(components, nil)
	if err != nil {
		return clusterctlProvider{}, err
	}
	return clusterctlProvider{"lodia", clusterctlv1.ControlPlaneProviderType, release.Version, pinned, metadata}, nil
}
