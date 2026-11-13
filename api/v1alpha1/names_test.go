// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"os"
	"path/filepath"
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
