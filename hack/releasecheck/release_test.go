// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

const root = "../.."

type step struct {
	ID              string            `json:"id"`
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
	Env         map[string]string `json:"env"`
	Permissions map[string]string `json:"permissions"`
	Jobs        map[string]struct {
		Needs       any               `json:"needs"`
		Env         map[string]string `json:"env"`
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
		"release":  {"contents": "write", "packages": "write", "actions": "read", "id-token": "write", "attestations": "write"},
		"cleanup":  {"contents": "write"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("permissions (-want +got):\n%s", diff)
	}
}

func TestTheReleaseWorkflowSetsNoEnvironmentAboveItsSteps(t *testing.T) {
	w := readRelease(t)

	if w.Env != nil {
		t.Errorf("the workflow sets the environment %v, which every make call of the release reads", w.Env)
	}
	for name, job := range w.Jobs {
		if job.Env != nil {
			t.Errorf("the job %s sets the environment %v, which every make call of it reads", name, job.Env)
		}
	}
}

func TestTheReleaseWorkflowPullsTheImageThatTheMakefileChecks(t *testing.T) {
	var order []string
	var withs []map[string]any
	runs := map[string]string{}
	envs := map[string]map[string]string{}
	ids := map[string]string{}
	for _, step := range readRelease(t).Jobs["release"].Steps {
		if step.If != nil || step.ContinueOnError != nil {
			t.Errorf("the step %q has an if or a continue-on-error, so a failed step may not stop the job", step.Name)
		}
		switch {
		case step.Name != "":
			order = append(order, step.Name)
			runs[step.Name] = step.Run
			envs[step.Name] = step.Env
			if step.ID != "" {
				ids[step.Name] = step.ID
			}
		case step.Uses != "":
			action, _, _ := strings.Cut(step.Uses, "@")
			order = append(order, action)
			withs = append(withs, step.With)
		default:
			t.Errorf("an unnamed step that runs %q", step.Run)
		}
	}

	const (
		checkout, setupGo, cosign, attest = "actions/checkout", "actions/setup-go", "sigstore/cosign-installer", "actions/attest-build-provenance"
		tagCheck, login, render, pull     = "Check the tag", "Log in to the registry", "Push the image and render the components YAML", "Check the image and pull it without credentials"
		sign, verifySigned, nameImage     = "Sign the image and the release files", "Verify the signatures as a user does", "Name the image by its digest"
		verifyAttested, publish           = "Verify the attestations as a user does", "Publish the release"
	)
	want := []string{checkout, setupGo, cosign, tagCheck, login, render, pull, sign, verifySigned, nameImage, attest, attest, verifyAttested, publish}
	if diff := cmp.Diff(want, order); diff != "" {
		t.Errorf("the steps of the release job (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]map[string]any{
		{"fetch-depth": float64(0), "persist-credentials": false},
		{"go-version-file": "go.mod"},
		{"cosign-release": "v3.1.4"},
		{"subject-name": "${{ steps.image.outputs.name }}", "subject-digest": "${{ steps.image.outputs.digest }}", "push-to-registry": true, "create-storage-record": false},
		{"subject-path": "out/control-plane-components.yaml\nmetadata.yaml\n"},
	}, withs); diff != "" {
		t.Errorf("the inputs of the unnamed steps, in order (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{nameImage: "image"}, ids); diff != "" {
		t.Errorf("the ids of the named steps, which the attestation's inputs name (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]map[string]string{
		tagCheck:       {"GITHUB_TOKEN": "${{ github.token }}"},
		login:          {"GITHUB_TOKEN": "${{ github.token }}"},
		render:         nil,
		pull:           nil,
		sign:           nil,
		verifySigned:   nil,
		nameImage:      nil,
		verifyAttested: {"GH_TOKEN": "${{ github.token }}"},
		publish:        {"GH_TOKEN": "${{ github.token }}"},
	}, envs); diff != "" {
		t.Errorf("the environments of the named steps (-want +got):\n%s", diff)
	}
	for name, want := range map[string]string{
		tagCheck: `go run ./hack/releasecheck tag --commit="$GITHUB_SHA" "$GITHUB_REF_NAME"`,
		login:    `make release-login`,
		render:   `make release-components IMAGE_TAG="$GITHUB_REF_NAME"`,
		pull: `image="$(make --no-print-directory release-image IMAGE_TAG="$GITHUB_REF_NAME")"` + "\n" +
			`DOCKER_CONFIG="$(mktemp -d)" docker pull "$image"` + "\n",
		sign:         `make release-sign IMAGE_TAG="$GITHUB_REF_NAME"`,
		verifySigned: `make release-verify-signatures IMAGE_TAG="$GITHUB_REF_NAME"`,
		nameImage: `image="$(make --no-print-directory release-image IMAGE_TAG="$GITHUB_REF_NAME")"` + "\n" +
			`echo "name=${image%:*@*}" >> "$GITHUB_OUTPUT"` + "\n" +
			`echo "digest=${image##*@}" >> "$GITHUB_OUTPUT"` + "\n",
		verifyAttested: `make release-verify-attestations IMAGE_TAG="$GITHUB_REF_NAME"`,
		publish: "flags=()\n" +
			`if [[ "$GITHUB_REF_NAME" == *-* ]]; then flags+=(--prerelease); fi` + "\n" +
			`gh release create "$GITHUB_REF_NAME" --verify-tag --title "$GITHUB_REF_NAME" --generate-notes "${flags[@]}" out/control-plane-components.yaml metadata.yaml out/control-plane-components.yaml.sigstore.json out/metadata.yaml.sigstore.json` + "\n",
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

	out = strings.TrimSpace(out)
	if want := `printf '%s' "$GITHUB_TOKEN" | `; !strings.HasPrefix(out, want) {
		t.Errorf("want the login to start with %q in:\n%s", want, out)
	}
	if want := ` login ghcr.io --username "$GITHUB_ACTOR" --password-stdin`; !strings.HasSuffix(out, want) {
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

func TestTheMakefileSignsAndVerifiesTheImageAndTheFilesOfTheRelease(t *testing.T) {
	const (
		tag      = "v0.2.0-rc.1"
		signer   = "--certificate-identity https://github.com/lodia-io/cluster-api-provider-lodia/.github/workflows/release.yml@refs/tags/" + tag + " --certificate-oidc-issuer https://token.actions.githubusercontent.com"
		image    = imageRepository + "@" + digest
		attested = "-R lodia-io/cluster-api-provider-lodia --signer-workflow lodia-io/cluster-api-provider-lodia/.github/workflows/release.yml --source-ref refs/tags/" + tag
	)
	platformA, platformB := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	index := `{"schemaVersion":2,"manifests":[{"digest":"` + platformA + `"},{"digest":"` + platformB + `"}]}`
	tagged := imageRepository + ":" + tag + "@" + digest
	dir := t.TempDir()
	bin := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		`{ printf '%s' "${0##*/}"; printf ' %s' "$@"; echo; } >> "$CALLS"` + "\n" +
		`[ "$(wc -l < "$CALLS" | tr -d ' ')" = "$FAIL_AT" ] && exit 1` + "\n" +
		`[ "${0##*/}" = crane ] && printf '%s' "$INDEX"` + "\n" +
		"exit 0\n"
	for _, tool := range []string{"cosign", "gh", "crane"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CALLS", calls)
	components := filepath.Join(dir, "control-plane-components.yaml")
	bundle := func(file string) string { return filepath.Join(dir, file+".sigstore.json") }
	lines := func(calls ...string) string {
		if len(calls) == 0 {
			return ""
		}
		return strings.Join(calls, "\n") + "\n"
	}
	run := func(t *testing.T, target, named, listed string, refuseCall int) (string, string, error) {
		t.Helper()
		if err := os.WriteFile(components, []byte(deployment([]string{named}, nil)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(calls); err != nil {
			t.Fatal(err)
		}
		refuse := ""
		if refuseCall > 0 {
			refuse = strconv.Itoa(refuseCall)
		}
		t.Setenv("FAIL_AT", refuse)
		t.Setenv("INDEX", listed)
		_, stderr, err := runMake(t, target, "RELEASE_DIR="+dir, "IMAGE_TAG="+tag, "CRANE="+filepath.Join(bin, "crane"))
		got, readErr := os.ReadFile(calls)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		return string(got), stderr, err
	}

	recipes := []struct {
		target string
		calls  []string
	}{
		{"release-sign", []string{
			"cosign sign --yes --recursive " + image,
			"cosign sign-blob --yes --bundle " + bundle("control-plane-components.yaml") + " " + components,
			"cosign sign-blob --yes --bundle " + bundle("metadata.yaml") + " metadata.yaml",
		}},
		{"release-verify-signatures", []string{
			"cosign verify " + signer + " " + image,
			"crane manifest " + image,
			"cosign verify " + signer + " " + imageRepository + "@" + platformA,
			"cosign verify " + signer + " " + imageRepository + "@" + platformB,
			"cosign verify-blob --bundle " + bundle("control-plane-components.yaml") + " " + signer + " " + components,
			"cosign verify-blob --bundle " + bundle("metadata.yaml") + " " + signer + " metadata.yaml",
		}},
		{"release-verify-attestations", []string{
			"gh attestation verify oci://" + image + " " + attested,
			"gh attestation verify " + components + " " + attested,
			"gh attestation verify metadata.yaml " + attested,
		}},
	}
	for _, tt := range recipes {
		t.Run(tt.target, func(t *testing.T) {
			got, stderr, err := run(t, tt.target, tagged, index, 0)
			if got != lines(tt.calls...) || err != nil {
				t.Errorf("ran %q and failed with %v and %q, want %q", got, err, stderr, lines(tt.calls...))
			}
			for refused := 1; refused <= len(tt.calls); refused++ {
				got, _, err := run(t, tt.target, tagged, index, refused)
				if got != lines(tt.calls[:refused]...) || err == nil {
					t.Errorf("with %q refused, ran %q and failed with %v, want the first %d calls and a failure", tt.calls[refused-1], got, err, refused)
				}
			}
			got, stderr, err = run(t, tt.target, imageRepository+":"+tag, index, 0)
			if got != "" || err == nil || !strings.Contains(stderr, "names no digest") {
				t.Errorf("for an image without a digest ran %q and failed with %v and %q, want no call and \"names no digest\"", got, err, stderr)
			}
		})
	}

	for _, listed := range []string{`{"manifests":[]}`, `{}`, ``} {
		t.Run("release-verify-signatures with the index "+listed, func(t *testing.T) {
			got, _, err := run(t, "release-verify-signatures", tagged, listed, 0)
			if want := lines(recipes[1].calls[:2]...); got != want || err == nil {
				t.Errorf("ran %q and failed with %v, want %q and a failure", got, err, want)
			}
		})
	}
}

func TestTheReleaseWorkflowPinsEveryActionToACommitWithItsVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`^\s*- uses: [A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$`)

	actions := 0
	for number, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "uses:") {
			continue
		}
		actions++
		if !pinned.MatchString(line) {
			t.Errorf("line %d, %q, does not pin an action to a full commit SHA with its version in a comment", number+1, strings.TrimSpace(line))
		}
	}
	if actions == 0 {
		t.Error("the release workflow uses no action, so the check found nothing to pin")
	}
}
