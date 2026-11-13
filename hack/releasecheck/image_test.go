// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"
)

const (
	imageRepository = "ghcr.io/lodia-io/cluster-api-provider-lodia"
	digest          = "sha256:4e66ff95b80a9e40fc8bacc3113c5ce9caff583a4d449cff1aa101fc63196348"
	pinned          = imageRepository + ":v0.1.0@" + digest
)

func deployment(containers, initContainers []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: manager\nspec:\n  template:\n    spec:\n")
	for _, group := range []struct {
		key    string
		images []string
	}{{"containers", containers}, {"initContainers", initContainers}} {
		if len(group.images) == 0 {
			continue
		}
		fmt.Fprintf(&b, "      %s:\n", group.key)
		for i, image := range group.images {
			fmt.Fprintf(&b, "      - name: c%d\n        image: %s\n", i, image)
		}
	}
	return b.String()
}

func TestTheImageCheck(t *testing.T) {
	schema := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: things.example.test\nspec:\n  versions:\n  - schema:\n      openAPIV3Schema:\n        properties:\n          image:\n            type: string\n            description: the image\n"
	tests := []struct {
		name       string
		components string
		want       string
	}{
		{name: "the one image by digest, tagged", components: schema + "---\n" + deployment([]string{pinned}, nil)},
		{name: "the same image in a container and an init container", components: deployment([]string{pinned}, []string{pinned})},
		{name: "a component without an image", components: schema, want: "names no image"},
		{name: "nothing at all", components: "", want: "names no image"},
		{name: "an image without a digest", components: deployment([]string{imageRepository + ":v0.1.0"}, nil), want: "names no digest"},
		{name: "an image by digest without a tag", components: deployment([]string{imageRepository + "@" + digest}, nil), want: "is not tagged v0.1.0"},
		{name: "an image with another tag", components: deployment([]string{imageRepository + ":latest@" + digest}, nil), want: "is not tagged v0.1.0"},
		{name: "an image in another repository", components: deployment([]string{"ghcr.io/lodia-io/other:v0.1.0@" + digest}, nil), want: "is not in " + imageRepository},
		{name: "an image in another registry", components: deployment([]string{"example.test/lodia-io/cluster-api-provider-lodia:v0.1.0@" + digest}, nil), want: "is not in " + imageRepository},
		{name: "an image below the repository, as ko names it without --bare", components: deployment([]string{imageRepository + "/cmd/cluster-api-provider-lodia:v0.1.0@" + digest}, nil), want: "is not in " + imageRepository},
		{name: "a ko reference that was not resolved", components: deployment([]string{"ko://go.lodia.io/cluster-api-provider-lodia/cmd/cluster-api-provider-lodia"}, nil), want: "not a reference that clusterctl parses"},
		{name: "a digest that is too short", components: deployment([]string{imageRepository + ":v0.1.0@sha256:4e66ff95"}, nil), want: "not a reference that clusterctl parses"},
		{name: "two images", components: deployment([]string{pinned, "ghcr.io/lodia-io/sidecar:v0.1.0@" + digest}, nil), want: "names 2 images"},
		{name: "an init container of another image", components: deployment([]string{pinned}, []string{"ghcr.io/lodia-io/init:v0.1.0@" + digest}), want: "names 2 images"},
		{name: "a second image in another kind", components: deployment([]string{pinned}, nil) + "---\napiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: job\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n          - name: c\n            image: busybox\n", want: "names 2 images"},
		{name: "a document that is not YAML", components: "a: [b\n", want: "the components YAML:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkImage([]byte(tt.components), imageRepository, "v0.1.0")

			if tt.want == "" {
				if err != nil {
					t.Fatalf("want the image accepted, got %v", err)
				}
				if got != pinned {
					t.Errorf("got %q, want %q", got, pinned)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error containing %q, got %v", tt.want, err)
			}
		})
	}
}
