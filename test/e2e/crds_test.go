// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

const versionSetupEnvtestRejects = "not-a-version"

var assetsOfEnvtest = regexp.MustCompile(`^assets="\$\(go tool setup-envtest use ` + versionSetupEnvtestRejects + ` -p path\)" && KUBEBUILDER_ASSETS="\$assets" go test \./\.\.\.$`)

func requireVerifyToRunTheTestsOfTheModuleWithTheAssetsOfEnvtest(plan string) error {
	if !slices.ContainsFunc(strings.Split(plan, "\n"), assetsOfEnvtest.MatchString) {
		return errors.New("make -n verify has no line that runs the go test of every package of the module with KUBEBUILDER_ASSETS taken from setup-envtest for the version of ENVTEST_K8S_VERSION, and only when setup-envtest succeeds")
	}
	return nil
}

func TestMakeVerifyRunsTheTestsOfTheModuleWithTheAssetsOfEnvtest(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "make", "--no-print-directory", "verify", "ENVTEST_K8S_VERSION="+versionSetupEnvtestRejects)
	cmd.Dir = root
	if err := requireVerifyToRunTheTestsOfTheModuleWithTheAssetsOfEnvtest(recipeOf(t, cmd)); err != nil {
		t.Fatal(err)
	}
}

func TestAVerifyThatRunsNoTestsOfTheModuleWithTheAssetsOfEnvtestIsRefused(t *testing.T) {
	const (
		version = versionSetupEnvtestRejects
		module  = `assets="$(go tool setup-envtest use ` + version + ` -p path)" && KUBEBUILDER_ASSETS="$assets" go test ./...` + "\n"
		api     = "cd api && go test ./...\n"
		other   = "go mod tidy -diff\ngolangci-lint run ./...\n"
	)
	cases := []struct {
		name, plan, want string
	}{
		{"the assets for the module and none for the API's", other + module + api, ""},
		{"the assets for the module after the API's", other + api + module, ""},
		{"a version of Kubernetes written out in place of ENVTEST_K8S_VERSION", other + strings.Replace(module, version, "1.35.0", 1) + api, "has no line"},
		{"no tests", other + api, "has no line"},
		{"no plan", "", "has no line"},
		{"an empty assignment", other + "KUBEBUILDER_ASSETS= go test ./...\n" + api, "has no line"},
		{"an assignment that runs go test whether setup-envtest succeeds or not", other + `KUBEBUILDER_ASSETS="$(go tool setup-envtest use ` + version + ` -p path)" go test ./...` + "\n" + api, "has no line"},
		{"go test after a semicolon, whatever setup-envtest did", other + `assets="$(go tool setup-envtest use ` + version + ` -p path)"; KUBEBUILDER_ASSETS="$assets" go test ./...` + "\n" + api, "has no line"},
		{"assets that setup-envtest did not give", other + `assets="/opt/envtest" && KUBEBUILDER_ASSETS="$assets" go test ./...` + "\n" + api, "has no line"},
		{"a variable that the test does not get", other + `assets="$(go tool setup-envtest use ` + version + ` -p path)" && KUBEBUILDER_ASSETS= go test ./...` + "\n" + api, "has no line"},
		{"a part of the module", other + strings.Replace(module, "./...", "./internal/...", 1) + api, "has no line"},
		{"the assets for the API's module alone", other + "cd api && " + module + "go test ./...\n", "has no line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireVerifyToRunTheTestsOfTheModuleWithTheAssetsOfEnvtest(c.plan)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

var ignoreInTheMakefile = regexp.MustCompile(`(?m)^ *\.IGNORE\b`)

func requireTheMakefileToHaveNoIgnore(makefile []byte) error {
	if ignoreInTheMakefile.Match(makefile) {
		return errors.New("the Makefile has .IGNORE, with which make goes on past the failed lines of the recipes that it names, or of all of them: make verify passes whatever they find, and the verify job of ci.yml sees only the failure of the test rule")
	}
	return nil
}

func TestTheMakefileHasNoIgnore(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := requireTheMakefileToHaveNoIgnore(makefile); err != nil {
		t.Fatal(err)
	}
}

func TestAMakefileWithIgnoreIsRefused(t *testing.T) {
	const rules = "lint:\n\tgolangci-lint run ./...\ntest:\n\tgo test ./...\n"
	cases := []struct {
		name, makefile, want string
	}{
		{"no .IGNORE", rules, ""},
		{"a comment that names .IGNORE", "# .IGNORE: test\n" + rules, ""},
		{"a recipe line that names .IGNORE", rules + "\techo .IGNORE: test\n", ""},
		{".IGNORE for the test target", ".IGNORE: test\n" + rules, "has .IGNORE"},
		{".IGNORE for the lint target", ".IGNORE: lint\n" + rules, "has .IGNORE"},
		{".IGNORE for the verify target", ".IGNORE: verify\n" + rules, "has .IGNORE"},
		{".IGNORE for every target", rules + ".IGNORE:\n", "has .IGNORE"},
		{".IGNORE with a space before its colon", ".IGNORE : lint\n" + rules, "has .IGNORE"},
		{".IGNORE after a space", " .IGNORE: lint\n" + rules, "has .IGNORE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireTheMakefileToHaveNoIgnore([]byte(c.makefile))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
