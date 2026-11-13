// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

const root = "../.."

type step struct {
	Name            string            `json:"name"`
	Uses            string            `json:"uses"`
	With            map[string]any    `json:"with"`
	Shell           string            `json:"shell"`
	Env             map[string]string `json:"env"`
	Run             string            `json:"run"`
	If              any               `json:"if"`
	ContinueOnError any               `json:"continue-on-error"`
}

type workflow struct {
	On          any               `json:"true"`
	Permissions map[string]string `json:"permissions"`
	Jobs        map[string]struct {
		Needs       any               `json:"needs"`
		If          string            `json:"if"`
		Permissions map[string]string `json:"permissions"`
		Steps       []step            `json:"steps"`
	} `json:"jobs"`
}

func readRelease(t *testing.T) workflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func runMake(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	if _, err := os.ReadFile(filepath.Join(root, "Makefile")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "make", append([]string{"--no-print-directory"}, args...)...)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestTheReleaseWorkflowGrantsOnlyWhatItUses(t *testing.T) {
	w := readRelease(t)

	got := map[string]map[string]string{"workflow": w.Permissions}
	for name, job := range w.Jobs {
		got[name] = job.Permissions
	}
	want := map[string]map[string]string{
		"workflow": {"contents": "read"},
		"release":  {"contents": "write", "packages": "write", "actions": "read"},
		"cleanup":  {"contents": "write"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("permissions (-want +got):\n%s", diff)
	}
}

func TestTheReleaseWorkflowPullsTheImageThatTheMakefileChecks(t *testing.T) {
	var names, uses []string
	runs := map[string]string{}
	envs := map[string]map[string]string{}
	withs := map[string]map[string]any{}
	for _, step := range readRelease(t).Jobs["release"].Steps {
		if step.If != nil || step.ContinueOnError != nil {
			t.Errorf("the step %q has an if or a continue-on-error, so a failed step may not stop the job", step.Name)
		}
		switch {
		case step.Name != "":
			names = append(names, step.Name)
			runs[step.Name] = step.Run
			envs[step.Name] = step.Env
		case step.Uses != "":
			action, _, _ := strings.Cut(step.Uses, "@")
			uses = append(uses, action)
			withs[action] = step.With
		default:
			t.Errorf("an unnamed step that runs %q", step.Run)
		}
	}

	const tagCheck, login, render, pull, publish = "Check the tag", "Log in to the registry", "Push the image and render the components YAML", "Check the image and pull it without credentials", "Publish the release"
	if want := []string{"actions/checkout", "actions/setup-go"}; !slices.Equal(uses, want) {
		t.Errorf("the unnamed steps use %v, want %v", uses, want)
	}
	if diff := cmp.Diff(map[string]map[string]any{
		"actions/checkout": {"fetch-depth": float64(0), "persist-credentials": false},
		"actions/setup-go": {"go-version-file": "go.mod"},
	}, withs); diff != "" {
		t.Errorf("the inputs of the unnamed steps (-want +got):\n%s", diff)
	}
	want := []string{tagCheck, login, render, pull, publish}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("the named steps of the release job (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]map[string]string{
		tagCheck: {"GITHUB_TOKEN": "${{ github.token }}"},
		login:    {"GITHUB_TOKEN": "${{ github.token }}"},
		render:   nil,
		pull:     nil,
		publish:  {"GH_TOKEN": "${{ github.token }}"},
	}, envs); diff != "" {
		t.Errorf("the environments of the named steps (-want +got):\n%s", diff)
	}
	for name, want := range map[string]string{
		tagCheck: `go run ./hack/releasecheck tag --commit="$GITHUB_SHA" "$GITHUB_REF_NAME"`,
		login:    `make release-login`,
		render:   `make release-components IMAGE_TAG="$GITHUB_REF_NAME"`,
		pull: `image="$(make --no-print-directory release-image IMAGE_TAG="$GITHUB_REF_NAME")"` + "\n" +
			`DOCKER_CONFIG="$(mktemp -d)" docker pull "$image"` + "\n",
		publish: "flags=()\n" +
			`if [[ "$GITHUB_REF_NAME" == *-* ]]; then flags+=(--prerelease); fi` + "\n" +
			`gh release create "$GITHUB_REF_NAME" --verify-tag --title "$GITHUB_REF_NAME" --generate-notes "${flags[@]}" out/control-plane-components.yaml metadata.yaml` + "\n",
	} {
		if runs[name] != want {
			t.Errorf("the step %q runs %q, want %q", name, runs[name], want)
		}
	}
}

func TestTheReleaseWorkflowStartsOnEveryTagAndCleansUpUnlessTheReleaseSucceeds(t *testing.T) {
	w := readRelease(t)

	if diff := cmp.Diff(map[string]any{"push": map[string]any{"tags": []any{"v*"}}}, w.On); diff != "" {
		t.Errorf("the trigger (-want +got):\n%s", diff)
	}
	cleanup := w.Jobs["cleanup"]
	if cleanup.Needs != "release" || cleanup.If != "always() && needs.release.result != 'success'" {
		t.Errorf("the cleanup job needs %v and runs if %q", cleanup.Needs, cleanup.If)
	}
	want := []step{{
		Name:  "Delete the draft releases of the tag",
		Shell: "bash",
		Env:   map[string]string{"GH_TOKEN": "${{ github.token }}"},
		Run: `gh api --paginate "repos/$GITHUB_REPOSITORY/releases" > releases.json` + "\n" +
			`ids="$(jq -r --arg tag "$GITHUB_REF_NAME" '.[] | select(.draft and .tag_name == $tag) | .id' releases.json)"` + "\n" +
			`for id in $ids; do` + "\n" +
			`  gh api --method DELETE "repos/$GITHUB_REPOSITORY/releases/$id"` + "\n" +
			`  echo "deleted the draft release $id of $GITHUB_REF_NAME"` + "\n" +
			`done` + "\n",
	}}
	if diff := cmp.Diff(want, cleanup.Steps); diff != "" {
		t.Errorf("the steps of the cleanup job (-want +got):\n%s", diff)
	}
}

func TestTheReleasePushesTheImageItRenders(t *testing.T) {
	out, stderr, err := runMake(t, "-n", "release-components", "IMAGE_TAG=v0.1.0")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}

	if want := " --platform=linux/amd64,linux/arm64 --push=true --tags=v0.1.0 "; !strings.Contains(out, want) {
		t.Errorf("want the release's render to contain %q in:\n%s", want, out)
	}
}

func TestTheReleaseLogsInToTheRegistryOfTheImage(t *testing.T) {
	out, stderr, err := runMake(t, "-n", "release-login")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}

	if want := ` login ghcr.io --username "$GITHUB_ACTOR" --password-stdin`; !strings.HasSuffix(strings.TrimSpace(out), want) {
		t.Errorf("want the login to end with %q in:\n%s", want, out)
	}
}

func TestVerifyChecksTheImageItRenders(t *testing.T) {
	out, stderr, err := runMake(t, "-n", "verify")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}

	render := strings.Index(out, "--platform=linux/amd64,linux/arm64 --push=false --tags=v0.0.0-verify")
	check := strings.Index(out, "go run ./hack/releasecheck image --repository=ghcr.io/lodia-io/cluster-api-provider-lodia --tag=v0.0.0-verify out/control-plane-components.yaml")
	if render < 0 || check < render {
		t.Errorf("want the render of the components YAML with the tag, then the check of its image with the same tag; the render is at %d and the check at %d of:\n%s", render, check, out)
	}
}

func TestVerifyRunsTheMetadataCheck(t *testing.T) {
	out, stderr, err := runMake(t, "-n", "verify")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}

	if !slices.Contains(strings.Split(out, "\n"), "go run ./hack/releasecheck metadata") {
		t.Errorf("want the metadata check as a line of its own in:\n%s", out)
	}
}

func TestReleaseCheckFetchesThenChecksTheCommit(t *testing.T) {
	out, stderr, err := runMake(t, "-n", "release-check", "TAG=v0.1.0", "COMMIT=abc123")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}

	fetch := strings.Index(out, "git fetch --quiet --tags origin main\n")
	check := strings.Index(out, `GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token)}" go run ./hack/releasecheck tag --commit="abc123" "v0.1.0"`)
	if fetch < 0 || check < fetch {
		t.Errorf("want the fetch of origin/main and the tags, then the check of COMMIT with the token of the environment or of gh; the fetch is at %d and the check at %d of:\n%s", fetch, check, out)
	}
}

func TestTheMakefileChecksTheImageOfTheRelease(t *testing.T) {
	dir := t.TempDir()
	components := filepath.Join(dir, "control-plane-components.yaml")
	for _, tt := range []struct {
		name, image, wantOut, wantErr string
	}{
		{name: "the image the release pushes", image: pinned, wantOut: pinned + "\n"},
		{name: "an image without a digest", image: imageRepository + ":v0.1.0", wantErr: "names no digest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(components, []byte(deployment([]string{tt.image}, nil)), 0o644); err != nil {
				t.Fatal(err)
			}

			out, stderr, err := runMake(t, "release-image", "RELEASE_DIR="+dir, "IMAGE_TAG=v0.1.0")

			if out != tt.wantOut || (err != nil) != (tt.wantErr != "") || !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("printed %q and failed with %v and %q, want %q and %q", out, err, stderr, tt.wantOut, tt.wantErr)
			}
		})
	}
}
