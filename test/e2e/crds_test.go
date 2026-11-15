// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

var capdKinds = []string{"DevCluster", "DevMachineTemplate"}

func moduleDir(t *testing.T, module string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		t.Fatalf("find %s: %v", module, err)
	}
	return strings.TrimSpace(string(out))
}

func TestEveryObjectOfTheTestdataIsAcceptedByTheCRDOfItsKind(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test")
	}
	capi := moduleDir(t, "sigs.k8s.io/cluster-api")
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(capi, "core", "config", "crd", "bases"),
			filepath.Join(capi, "bootstrap", "kubeadm", "config", "crd", "bases"),
			filepath.Join(moduleDir(t, "go.lodia.io/lodia/api"), "crds"),
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
		UseExistingCluster:    new(false),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	k8s, err := client.NewWithWatch(cfg, client.Options{Scheme: controller.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	var skipped []string
	dryRun := interceptor.NewClient(k8s, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.Namespace); ok {
			return client.IgnoreAlreadyExists(c.Create(ctx, obj, opts...))
		}
		if kind := obj.GetObjectKind().GroupVersionKind().Kind; slices.Contains(capdKinds, kind) {
			skipped = append(skipped, kind)
			return nil
		}
		return c.Create(ctx, obj, append(opts, client.DryRunAll)...)
	}})
	for _, file := range renderedFiles(t) {
		for _, version := range []string{previousKubernetes, newestKubernetes} {
			t.Run(file+" at "+version, func(t *testing.T) {
				manifest, err := render(file, testdataVariables(scaleKey.Name, version))
				if err != nil {
					t.Fatal(err)
				}
				if err := createAll(t.Context(), dryRun, manifest); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	slices.Sort(skipped)
	if got := slices.Compact(skipped); !slices.Equal(got, capdKinds) {
		t.Errorf("the kinds left out are %v, want CAPD's %v", got, capdKinds)
	}
}
