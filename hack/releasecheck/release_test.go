// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

const root = "../.."

type workflow struct {
	Permissions map[string]string `json:"permissions"`
	Jobs        map[string]struct {
		Permissions map[string]string `json:"permissions"`
		Steps       []struct {
			Name            string `json:"name"`
			Run             string `json:"run"`
			If              any    `json:"if"`
			ContinueOnError any    `json:"continue-on-error"`
		} `json:"steps"`
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
	// go test caches a result by the files its process opens, https://github.com/golang/go/blob/go1.26.10/src/cmd/go/internal/test/test.go#L2008-L2060, and make opens the Makefile in a child.
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
	var names []string
	runs := map[string]string{}
	for _, step := range readRelease(t).Jobs["release"].Steps {
		if step.If != nil || step.ContinueOnError != nil {
			t.Errorf("the step %q has an if or a continue-on-error, so a failed step may not stop the job", step.Name)
		}
		if step.Name != "" {
			names = append(names, step.Name)
			runs[step.Name] = step.Run
		}
	}

	const render, pull, publish = "Push the image and render the components YAML", "Check the image and pull it without credentials", "Publish the release"
	want := []string{"Check the tag", "Log in to the registry", render, pull, publish}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("the named steps of the release job (-want +got):\n%s", diff)
	}
	if wantRender := `make release-components IMAGE_TAG="$GITHUB_REF_NAME"`; runs[render] != wantRender {
		t.Errorf("the render step runs %q, want %q", runs[render], wantRender)
	}
	wantPull := `image="$(make --no-print-directory release-image IMAGE_TAG="$GITHUB_REF_NAME")"` + "\n" +
		`DOCKER_CONFIG="$(mktemp -d)" docker pull "$image"` + "\n"
	if runs[pull] != wantPull {
		t.Errorf("the pull step runs %q, want %q", runs[pull], wantPull)
	}
}

func TestVerifyChecksTheImageItRenders(t *testing.T) {
	out, stderr, err := runMake(t, "-n", "verify")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}

	render := strings.Index(out, "--push=false --tags=v0.0.0-verify")
	check := strings.Index(out, "go run ./hack/releasecheck image --repository=ghcr.io/lodia-io/cluster-api-provider-lodia --tag=v0.0.0-verify out/control-plane-components.yaml")
	if render < 0 || check < render {
		t.Errorf("want the render of the components YAML with the tag, then the check of its image with the same tag; the render is at %d and the check at %d of:\n%s", render, check, out)
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
