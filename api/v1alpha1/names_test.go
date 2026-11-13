// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestTheDocsNameTheAnnotationAndTheReasonUsersActOn(t *testing.T) {
	for _, doc := range []string{"README.md", filepath.Join("docs", "design.md")} {
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
	for _, doc := range []string{"README.md", filepath.Join("docs", "design.md")} {
		raw, err := os.ReadFile(filepath.Join("..", "..", doc))
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
