// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package build_test

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const makeVerifyWithAVersionSetupEnvtestRejects = `if make verify ENVTEST_K8S_VERSION=` + versionSetupEnvtestRejects + `; then
  echo "make verify exited 0 with a version that setup-envtest rejects: the Makefile makes make go on past a failed line, or its test rule does not take ENVTEST_K8S_VERSION, so make verify passes whatever setup-envtest and the tests do"
  exit 1
fi
`

func requireTheVerifyJobToRunMakeVerifyWithAVersionSetupEnvtestRejectsAfterMakeVerify(steps []map[string]any) error {
	verify := slices.IndexFunc(steps, func(step map[string]any) bool {
		run, _ := step["run"].(string)
		return strings.TrimSpace(run) == "make verify"
	})
	if verify < 0 {
		return errors.New("the verify job has no step that runs make verify")
	}
	check := slices.IndexFunc(steps, func(step map[string]any) bool { return step["run"] == makeVerifyWithAVersionSetupEnvtestRejects })
	switch {
	case check < 0:
		return errors.New("the verify job has no step whose run is the script that fails the job when make verify exits 0 with a version that setup-envtest rejects: a Makefile that makes make ignore the failure of setup-envtest would pass make verify")
	case check < verify:
		return errors.New("the step of the verify job that runs make verify with a version that setup-envtest rejects comes before make verify, and has to follow it")
	}
	for _, key := range slices.Sorted(maps.Keys(steps[check])) {
		if key != "name" && key != "run" {
			return fmt.Errorf("the step of the verify job that runs make verify with a version that setup-envtest rejects has %s, and may have only a name and a run: the job would pass without the step failing it", key)
		}
	}
	return nil
}

func TestTheVerifyJobRunsMakeVerifyWithAVersionSetupEnvtestRejectsAfterMakeVerify(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var ci struct {
		Jobs map[string]struct {
			Steps []map[string]any `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		t.Fatal(err)
	}
	if err := requireTheVerifyJobToRunMakeVerifyWithAVersionSetupEnvtestRejectsAfterMakeVerify(ci.Jobs["verify"].Steps); err != nil {
		t.Fatal(err)
	}
}

func TestAVerifyJobThatDoesNotRunMakeVerifyWithAVersionSetupEnvtestRejectsAfterMakeVerifyIsRefused(t *testing.T) {
	run := func(script string) map[string]any { return map[string]any{"run": script} }
	with := func(key string, value any) map[string]any {
		return map[string]any{"run": makeVerifyWithAVersionSetupEnvtestRejects, key: value}
	}
	checkout := map[string]any{"uses": "actions/checkout"}
	verify := run("make verify")
	check := run(makeVerifyWithAVersionSetupEnvtestRejects)
	cases := []struct {
		name  string
		steps []map[string]any
		want  string
	}{
		{"the check right after make verify", []map[string]any{checkout, verify, check}, ""},
		{"the check after another step", []map[string]any{verify, run("echo done"), check}, ""},
		{"make verify with a newline", []map[string]any{run("make verify\n"), check}, ""},
		{"the check with a name", []map[string]any{verify, with("name", "Check")}, ""},
		{"no check", []map[string]any{checkout, verify}, "has no step whose run is"},
		{"make test with the Makefile's own version", []map[string]any{verify, run("make test")}, "has no step whose run is"},
		{"make test with the version that setup-envtest rejects", []map[string]any{verify, run("make test ENVTEST_K8S_VERSION=" + versionSetupEnvtestRejects)}, "has no step whose run is"},
		{"make verify with the Makefile's own version", []map[string]any{verify, run("make verify")}, "has no step whose run is"},
		{"a check whose failure is swallowed", []map[string]any{verify, run("make verify ENVTEST_K8S_VERSION=" + versionSetupEnvtestRejects + " || true")}, "has no step whose run is"},
		{"a check that never exits 1", []map[string]any{verify, run(strings.Replace(makeVerifyWithAVersionSetupEnvtestRejects, "  exit 1\n", "", 1))}, "has no step whose run is"},
		{"a check that continues on error", []map[string]any{verify, with("continue-on-error", true)}, "has continue-on-error"},
		{"a check with a condition", []map[string]any{verify, with("if", false)}, "has if"},
		{"a check with another shell", []map[string]any{verify, with("shell", "sh {0}")}, "has shell"},
		{"a check with an environment", []map[string]any{verify, with("env", map[string]any{"MAKEFLAGS": "-i"})}, "has env"},
		{"the check before make verify", []map[string]any{check, verify}, "comes before make verify"},
		{"no make verify", []map[string]any{check}, "has no step that runs make verify"},
		{"no steps", nil, "has no step that runs make verify"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireTheVerifyJobToRunMakeVerifyWithAVersionSetupEnvtestRejectsAfterMakeVerify(c.steps)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
