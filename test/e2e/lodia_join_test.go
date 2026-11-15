// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e && !upgrade

package e2e

import (
	"context"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

func lodiaProviders(ctx context.Context, cfg *envconf.Config) ([]clusterctlProvider, string, error) {
	lodia, err := lodiaProvider(ctx, cfg)
	return []clusterctlProvider{lodia}, "lodia", err
}
