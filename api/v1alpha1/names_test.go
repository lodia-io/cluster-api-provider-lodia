// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestTheDocsNameTheAnnotationAndTheReasonUsersActOn(t *testing.T) {
	for _, doc := range []string{
		filepath.Join("docs", "README.md"),
		filepath.Join("docs", "decisions", "hosted-control-planes-do-not-move.md"),
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{v1alpha1.UIDAnnotation, v1alpha1.CopyOfAnotherObjectReason} {
			if !strings.Contains(string(raw), "`"+name+"`") {
				t.Errorf("%s does not name `%s`", doc, name)
			}
		}
	}
}

func TestTheDocsNameNoOtherNameOfTheGroupThanTheAPIDefines(t *testing.T) {
	defined := []string{v1alpha1.UIDAnnotation, v1alpha1.Finalizer, v1alpha1.GroupVersion.String()}
	names := regexp.MustCompile("`(" + regexp.QuoteMeta(v1alpha1.GroupVersion.Group) + "/[^`]*)`")
	root := filepath.Join("..", "..")
	listed := exec.CommandContext(t.Context(), "git", "ls-files", "-z", "*.md")
	listed.Dir = root
	out, err := listed.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	docs := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(docs) < 3 {
		t.Fatalf("git ls-files lists %d Markdown files, want every tracked one", len(docs))
	}
	for _, doc := range docs {
		raw, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range names.FindAllStringSubmatch(string(raw), -1) {
			if !slices.Contains(defined, m[1]) {
				t.Errorf("%s names `%s`, which the API does not define", doc, m[1])
			}
		}
	}
}
