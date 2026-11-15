// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package build_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const root = "../.."

func recipeOf(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	cmd.Args = slices.Insert(cmd.Args, 1, "-n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", cmd.Args, err)
	}
	return string(out)
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
