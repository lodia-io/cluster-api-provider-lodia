// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"errors"
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
		for _, version := range []string{worldFrom, worldTo} {
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

func requireTheTestTargetToSetTheAssetsOfEnvtest(makefile []byte) error {
	for _, rule := range targetRule.FindAllSubmatch(makefile, -1) {
		if string(rule[1]) != "test" {
			continue
		}
		for line := range bytes.SplitSeq(rule[2], []byte("\n")) {
			if line = bytes.TrimSpace(line); bytes.HasPrefix(line, []byte("KUBEBUILDER_ASSETS=")) && bytes.Contains(line, []byte(" go test ./...")) {
				return nil
			}
		}
		return errors.New("no line of the test target sets KUBEBUILDER_ASSETS for the go test of every package of the module")
	}
	return errors.New("the Makefile has no target test")
}

func TestTheTestTargetThatMakeVerifyRunsSetsTheAssetsOfEnvtest(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireTheTestTargetToSetTheAssetsOfEnvtest(makefile); err != nil {
		t.Fatal(err)
	}
}

func TestATestTargetThatSetsNoAssetsForTheModuleIsRefused(t *testing.T) {
	const (
		module = "\tKUBEBUILDER_ASSETS=\"$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)\" go test ./...\n"
		api    = "\tcd api && go test ./...\n"
		bare   = "\tgo test ./...\n"
	)
	cases := []struct {
		name, makefile, want string
	}{
		{"the assets for the module and none for the API's", "test:\n" + module + api, ""},
		{"the assets for the module after the API's", "test:\n" + api + module, ""},
		{"a target after the test", "test:\n" + module + "components:\n" + bare, ""},
		{"no assets", "test:\n" + bare + api, "sets KUBEBUILDER_ASSETS"},
		{"the assets for the API's module alone", "test:\n" + bare + "\tcd api && KUBEBUILDER_ASSETS=x go test ./...\n", "sets KUBEBUILDER_ASSETS"},
		{"the assets for a part of the module", "test:\n\tKUBEBUILDER_ASSETS=x go test ./internal/...\n", "sets KUBEBUILDER_ASSETS"},
		{"the assets in another target", "verify:\n" + module + "test:\n" + bare, "sets KUBEBUILDER_ASSETS"},
		{"no test target", "verify:\n" + module, "no target test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireTheTestTargetToSetTheAssetsOfEnvtest([]byte(c.makefile))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
