// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

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

func kindCluster(kubeconfig string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(cfg.CurrentContext, "kind-"), nil
}
