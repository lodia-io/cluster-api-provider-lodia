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

var assetsOfEnvtest = regexp.MustCompile(`^assets="\$\(go tool setup-envtest use \S+ -p path\)" && KUBEBUILDER_ASSETS="\$assets" go test \./\.\.\.$`)

func requireVerifyToRunTheTestsOfTheModuleWithTheAssetsOfEnvtest(plan string) error {
	if !slices.ContainsFunc(strings.Split(plan, "\n"), assetsOfEnvtest.MatchString) {
		return errors.New("make -n verify has no line that runs the go test of every package of the module with KUBEBUILDER_ASSETS taken from setup-envtest, and only when setup-envtest succeeds")
	}
	return nil
}

func TestMakeVerifyRunsTheTestsOfTheModuleWithTheAssetsOfEnvtest(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "make", "--no-print-directory", "verify")
	cmd.Dir = root
	if err := requireVerifyToRunTheTestsOfTheModuleWithTheAssetsOfEnvtest(recipeOf(t, cmd)); err != nil {
		t.Fatal(err)
	}
}

func TestAVerifyThatRunsNoTestsOfTheModuleWithTheAssetsOfEnvtestIsRefused(t *testing.T) {
	const (
		module = `assets="$(go tool setup-envtest use 1.35.0 -p path)" && KUBEBUILDER_ASSETS="$assets" go test ./...` + "\n"
		api    = "cd api && go test ./...\n"
		other  = "go mod tidy -diff\ngolangci-lint run ./...\n"
	)
	cases := []struct {
		name, plan, want string
	}{
		{"the assets for the module and none for the API's", other + module + api, ""},
		{"the assets for the module after the API's", other + api + module, ""},
		{"another version of Kubernetes", other + strings.Replace(module, "1.35.0", "1.36.1", 1) + api, ""},
		{"no tests", other + api, "has no line"},
		{"no plan", "", "has no line"},
		{"an empty assignment", other + "KUBEBUILDER_ASSETS= go test ./...\n" + api, "has no line"},
		{"an assignment that runs go test whether setup-envtest succeeds or not", other + `KUBEBUILDER_ASSETS="$(go tool setup-envtest use 1.35.0 -p path)" go test ./...` + "\n" + api, "has no line"},
		{"go test after a semicolon, whatever setup-envtest did", other + `assets="$(go tool setup-envtest use 1.35.0 -p path)"; KUBEBUILDER_ASSETS="$assets" go test ./...` + "\n" + api, "has no line"},
		{"assets that setup-envtest did not give", other + `assets="/opt/envtest" && KUBEBUILDER_ASSETS="$assets" go test ./...` + "\n" + api, "has no line"},
		{"a variable that the test does not get", other + `assets="$(go tool setup-envtest use 1.35.0 -p path)" && KUBEBUILDER_ASSETS= go test ./...` + "\n" + api, "has no line"},
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

var (
	assetsLineOfTheTestRule = regexp.MustCompile(`^\tassets="\$\$\(go tool setup-envtest use \S+ -p path\)" && KUBEBUILDER_ASSETS="\$\$assets" go test \./\.\.\.$`)
	ruleThatIgnoresFailures = regexp.MustCompile(`(?m)^ *\.IGNORE\b`)
)

func requireTheTestRuleToStopWhenSetupEnvtestFails(makefile []byte) error {
	if ruleThatIgnoresFailures.Match(makefile) {
		return errors.New("the Makefile has .IGNORE, with which make goes on past a failed line, so make verify passes whatever setup-envtest and the tests do")
	}
	for _, rule := range targetRule.FindAllSubmatch(makefile, -1) {
		if string(rule[1]) != "test" {
			continue
		}
		for line := range bytes.SplitSeq(rule[2], []byte("\n")) {
			if assetsLineOfTheTestRule.Match(line) {
				return nil
			}
		}
		return errors.New("no line of the test rule has, right after its tab, the assets of setup-envtest and the go test of every package of the module that runs only when it succeeds: a - before it makes make ignore its failure")
	}
	return errors.New("the Makefile has no rule test")
}

func TestMakeVerifyRunsATestRuleThatStopsWhenSetupEnvtestFails(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireTheTestRuleToStopWhenSetupEnvtestFails(makefile); err != nil {
		t.Fatal(err)
	}
}

func TestAVerifyWhoseTestRuleMakeMayIgnoreAFailureOfIsRefused(t *testing.T) {
	const (
		module = "\tassets=\"$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)\" && KUBEBUILDER_ASSETS=\"$$assets\" go test ./...\n"
		api    = "\tcd api && go test ./...\n"
	)
	cases := []struct {
		name, makefile, want string
	}{
		{"the assets for the module and none for the API's", "test:\n" + module + api, ""},
		{"the assets after the API's", "test:\n" + api + module, ""},
		{"a comment on the rule", "test: ## unit and envtest tests\n" + module + api, ""},
		{"a target after the test", "test:\n" + module + "components:\n\t-go run ./test/components\n", ""},
		{"a version of Kubernetes written out", "test:\n" + strings.Replace(module, "$(ENVTEST_K8S_VERSION)", "1.35.0", 1) + api, ""},
		{"a comment that names .IGNORE", "# .IGNORE: test\ntest:\n" + module + api, ""},
		{"a recipe line that names .IGNORE", "test:\n" + module + "\techo .IGNORE: test\n", ""},
		{"a dash before the assets", "test:\n\t-" + module[1:] + api, "no line of the test rule"},
		{"a dash after an at sign", "test:\n\t@-" + module[1:] + api, "no line of the test rule"},
		{"an at sign after a dash", "test:\n\t-@" + module[1:] + api, "no line of the test rule"},
		{".IGNORE for the test target", ".IGNORE: test\n.PHONY: test\ntest:\n" + module + api, "has .IGNORE"},
		{".IGNORE for every target", "test:\n" + module + api + ".IGNORE:\n", "has .IGNORE"},
		{".IGNORE with a space before its colon", ".IGNORE : test\ntest:\n" + module + api, "has .IGNORE"},
		{".IGNORE after a space", " .IGNORE: test\ntest:\n" + module + api, "has .IGNORE"},
		{"no assets", "test:\n\tgo test ./...\n" + api, "no line of the test rule"},
		{"a semicolon where the && was", "test:\n\tassets=\"$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)\"; KUBEBUILDER_ASSETS=\"$$assets\" go test ./...\n" + api, "no line of the test rule"},
		{"a failure of go test that the line hides", "test:\n" + strings.Replace(module, "\n", " || true\n", 1) + api, "no line of the test rule"},
		{"a part of the module", "test:\n" + strings.Replace(module, "./...", "./internal/...", 1) + api, "no line of the test rule"},
		{"the assets in another rule", "verify:\n" + module + "test:\n" + api, "no line of the test rule"},
		{"no test rule", "verify:\n" + module, "no rule test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireTheTestRuleToStopWhenSetupEnvtestFails([]byte(c.makefile))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
