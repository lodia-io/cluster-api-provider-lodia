// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package build_test

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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

func readCI(t *testing.T) []byte {
	t.Helper()
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return workflow
}

func requireEveryJobToEndAtATimeout(workflow []byte) error {
	var ci struct {
		Jobs map[string]struct {
			TimeoutMinutes any `json:"timeout-minutes"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		return err
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(ci.Jobs)) {
		if ci.Jobs[id].TimeoutMinutes == nil {
			errs = append(errs, fmt.Errorf("the %s job of ci.yml has no timeout-minutes: a step that hangs would hold the runner until GitHub's own limit", id))
		}
	}
	return errors.Join(errs...)
}

func TestEveryJobOfCIEndsAtATimeout(t *testing.T) {
	if err := requireEveryJobToEndAtATimeout(readCI(t)); err != nil {
		t.Fatal(err)
	}
}

func TestAJobOfCIWithoutATimeoutIsRefused(t *testing.T) {
	cases := []struct {
		name, workflow, want string
	}{
		{"a timeout on every job", "jobs:\n  verify:\n    timeout-minutes: 30\n  e2e:\n    timeout-minutes: ${{ matrix.timeout }}\n", ""},
		{"no timeout on the first job", "jobs:\n  verify:\n    runs-on: x\n  e2e:\n    timeout-minutes: 74\n", "the verify job of ci.yml has no timeout-minutes"},
		{"no timeout on the second job", "jobs:\n  verify:\n    timeout-minutes: 30\n  e2e:\n    runs-on: x\n", "the e2e job of ci.yml has no timeout-minutes"},
		{"no job", "name: ci\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireEveryJobToEndAtATimeout([]byte(c.workflow))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

const (
	groupOfCI             = "ci-${{ github.event_name == 'pull_request' && github.ref || github.run_id }}"
	cancellationOfCI      = "${{ github.event_name == 'pull_request' }}"
	concurrencyOfCI       = "concurrency:\n  group: " + groupOfCI + "\n  cancel-in-progress: " + cancellationOfCI + "\n"
	whyMainIsNotCancelled = "a run of main must not be cancelled, since release-check refuses a cancelled run, and a pending run of a group is cancelled by the next one"
)

func requireOnlyPullRequestsToBeCancelled(workflow []byte) error {
	var ci struct {
		Concurrency map[string]any `json:"concurrency"`
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		return err
	}
	if ci.Concurrency == nil {
		return errors.New("ci.yml has no concurrency: a pull request that is pushed to again would go on with its superseded run")
	}
	var errs []error
	if got := ci.Concurrency["group"]; got != groupOfCI {
		errs = append(errs, fmt.Errorf("the concurrency group of ci.yml is %q, want %q, so that a push to main has a group of its own and a pull request one by its ref: %s", got, groupOfCI, whyMainIsNotCancelled))
	}
	if got := ci.Concurrency["cancel-in-progress"]; got != cancellationOfCI {
		errs = append(errs, fmt.Errorf("cancel-in-progress of ci.yml is %v, want %q: %s", got, cancellationOfCI, whyMainIsNotCancelled))
	}
	for _, key := range slices.Sorted(maps.Keys(ci.Concurrency)) {
		if key != "group" && key != "cancel-in-progress" {
			errs = append(errs, fmt.Errorf("the concurrency of ci.yml has %s, and may have only a group and cancel-in-progress", key))
		}
	}
	return errors.Join(errs...)
}

func TestCIOnlyCancelsSupersededRunsOfPullRequests(t *testing.T) {
	if err := requireOnlyPullRequestsToBeCancelled(readCI(t)); err != nil {
		t.Fatal(err)
	}
}

func TestACIThatCancelsARunOfMainIsRefused(t *testing.T) {
	cases := []struct {
		name, workflow, want string
	}{
		{"the group and cancellation that spare main", concurrencyOfCI, ""},
		{"no concurrency", "name: ci\n", "has no concurrency"},
		{"every run cancelled", "concurrency:\n  group: " + groupOfCI + "\n  cancel-in-progress: true\n", "cancel-in-progress of ci.yml is true"},
		{"no run cancelled", "concurrency:\n  group: " + groupOfCI + "\n  cancel-in-progress: false\n", "cancel-in-progress of ci.yml is false"},
		{"no cancel-in-progress", "concurrency:\n  group: " + groupOfCI + "\n", "cancel-in-progress of ci.yml is <nil>"},
		{"a group by ref for every event", "concurrency:\n  group: ci-${{ github.ref }}\n  cancel-in-progress: " + cancellationOfCI + "\n", "the concurrency group of ci.yml is"},
		{"a group by the workflow and the ref", "concurrency:\n  group: ${{ github.workflow }}-${{ github.ref }}\n  cancel-in-progress: " + cancellationOfCI + "\n", "the concurrency group of ci.yml is"},
		{"a group of one name", "concurrency:\n  group: ci\n  cancel-in-progress: " + cancellationOfCI + "\n", "the concurrency group of ci.yml is"},
		{"a group by head_ref", "concurrency:\n  group: ci-${{ github.head_ref || github.run_id }}\n  cancel-in-progress: " + cancellationOfCI + "\n", "the concurrency group of ci.yml is"},
		{"a queue of runs", concurrencyOfCI + "  queue: max\n", "has queue"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireOnlyPullRequestsToBeCancelled([]byte(c.workflow))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

var pinnedAction = regexp.MustCompile(`^\s*(?:- )?uses: [A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$`)

func unpinnedActions(workflow string) []string {
	var found []string
	for number, line := range strings.Split(workflow, "\n") {
		if strings.Contains(line, "uses:") && !pinnedAction.MatchString(line) {
			found = append(found, fmt.Sprintf("line %d, %q", number+1, strings.TrimSpace(line)))
		}
	}
	return found
}

func TestTheCIWorkflowPinsEveryActionToACommitWithItsVersion(t *testing.T) {
	workflow := string(readCI(t))
	if !strings.Contains(workflow, "uses:") {
		t.Fatal("ci.yml uses no action, so the check found nothing to pin")
	}
	for _, found := range unpinnedActions(workflow) {
		t.Errorf("%s does not pin an action to a full commit SHA with its version in a comment", found)
	}
}

func TestAnActionThatIsNotPinnedToACommitWithItsVersionIsRefused(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	cases := []struct {
		name, line string
		want       int
	}{
		{"a commit and its version", "      - uses: actions/checkout@" + sha + " # v7.0.1", 0},
		{"a step with a name before its action", "        uses: actions/checkout@" + sha + " # v7.0.1", 0},
		{"a path inside the repository of the action", "      - uses: github/codeql-action/init@" + sha + " # v4.1.0", 0},
		{"a tag", "      - uses: actions/checkout@v7", 1},
		{"a branch", "      - uses: actions/checkout@main", 1},
		{"a short commit", "      - uses: actions/checkout@3d3c42e # v7.0.1", 1},
		{"a commit without its version", "      - uses: actions/checkout@" + sha, 1},
		{"a commit with a comment that is not a version", "      - uses: actions/checkout@" + sha + " # checkout", 1},
		{"a commit with a version of two numbers", "      - uses: actions/checkout@" + sha + " # v7.0", 1},
		{"a commit in capitals", "      - uses: actions/checkout@" + strings.ToUpper(sha) + " # v7.0.1", 1},
		{"an action of the repository", "      - uses: ./.github/actions/setup", 1},
		{"a run that mentions nothing", "      - run: make verify", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if found := unpinnedActions(c.line); len(found) != c.want {
				t.Errorf("found %d unpinned actions in %q, want %d: %v", len(found), c.line, c.want, found)
			}
		})
	}
}
