// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

var imageLine = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]+)?image:[ \t]+)"?([^"\s]+)"?[ \t]*$`)

func pinImages(manifest []byte, digests map[string]string) ([]byte, []string) {
	var unpinned []string
	pinned := imageLine.ReplaceAllFunc(manifest, func(line []byte) []byte {
		match := imageLine.FindSubmatch(line)
		if bytes.Contains(match[2], []byte("@sha256:")) {
			return line
		}
		digest, ok := digests[string(match[2])]
		if !ok {
			unpinned = append(unpinned, string(match[2]))
			return line
		}
		return fmt.Appendf(nil, "%s%s@%s", match[1], match[2], digest)
	})
	return pinned, unpinned
}

func requireDigests(manifest []byte, digests map[string]string) ([]byte, error) {
	pinned, unpinned := pinImages(manifest, digests)
	if len(unpinned) > 0 {
		return nil, fmt.Errorf("images outside the suite's table: %s", strings.Join(unpinned, ", "))
	}
	tagged, err := imagesWithoutDigest(pinned)
	if err != nil {
		return nil, err
	}
	if len(tagged) > 0 {
		return nil, fmt.Errorf("images without a digest: %s", strings.Join(tagged, ", "))
	}
	return pinned, nil
}

func imagesWithoutDigest(manifest []byte) ([]string, error) {
	var images []string
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		var object any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		images = append(images, containerImagesWithoutDigest(object)...)
	}
	slices.Sort(images)
	return slices.Compact(images), nil
}

func containerImagesWithoutDigest(node any) []string {
	var images []string
	switch node := node.(type) {
	case map[string]any:
		for key, value := range node {
			if key == "containers" || key == "initContainers" {
				containers, _ := value.([]any)
				for _, item := range containers {
					if container, ok := item.(map[string]any); ok {
						if image, _ := container["image"].(string); !strings.Contains(image, "@sha256:") {
							images = append(images, image)
						}
					}
				}
			}
			images = append(images, containerImagesWithoutDigest(value)...)
		}
	case []any:
		for _, item := range node {
			images = append(images, containerImagesWithoutDigest(item)...)
		}
	}
	return images
}

func TestEveryImageLineIsPinned(t *testing.T) {
	digests := map[string]string{"example.io/a:v1": "sha256:aa", "example.io/b:v1": "sha256:bb"}
	cases := []struct {
		name, manifest, want string
		unpinned             []string
	}{
		{"a key", "        image: example.io/a:v1\n", "        image: example.io/a:v1@sha256:aa\n", nil},
		{"a quoted value", "image: \"example.io/a:v1\"\n", "image: example.io/a:v1@sha256:aa\n", nil},
		{"the first key of a list item", "      - image: example.io/b:v1\n", "      - image: example.io/b:v1@sha256:bb\n", nil},
		{"a list item with an image outside the table", "      - image: example.io/c:v1\n", "      - image: example.io/c:v1\n", []string{"example.io/c:v1"}},
		{"a key that only ends in image", "      - myimage: example.io/a:v1\n", "      - myimage: example.io/a:v1\n", nil},
		{"a reference that carries its digest", "        image: example.io/c:v1@sha256:cc\n", "        image: example.io/c:v1@sha256:cc\n", nil},
		{"a quoted reference that carries its digest", "image: \"example.io/c:v1@sha256:cc\"\n", "image: \"example.io/c:v1@sha256:cc\"\n", nil},
		{"a reference that carries a digest beside one outside the table", "image: example.io/c:v1@sha256:cc\nimage: example.io/d:v1\n", "image: example.io/c:v1@sha256:cc\nimage: example.io/d:v1\n", []string{"example.io/d:v1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, unpinned := pinImages([]byte(c.manifest), digests)
			if string(got) != c.want || !slices.Equal(unpinned, c.unpinned) {
				t.Fatalf("got %q and unpinned %v, want %q and %v", got, unpinned, c.want, c.unpinned)
			}
		})
	}
}

func TestEveryContainerImageIsPinnedOrRefused(t *testing.T) {
	digests := map[string]string{"example.io/a:v1": "sha256:aa"}
	pod := func(spec string) string {
		return "apiVersion: v1\nkind: Pod\nspec:\n" + spec
	}
	cases := []struct {
		name, manifest, want, wantErr string
	}{
		{"a container in the table", pod("  containers:\n  - name: c\n    image: example.io/a:v1\n"), pod("  containers:\n  - name: c\n    image: example.io/a:v1@sha256:aa\n"), ""},
		{"a container that carries its digest", pod("  containers:\n  - name: c\n    image: example.io/c:v1@sha256:cc\n"), pod("  containers:\n  - name: c\n    image: example.io/c:v1@sha256:cc\n"), ""},
		{"a container that carries its digest beside one outside the table", pod("  containers:\n  - name: c\n    image: example.io/c:v1@sha256:cc\n  - name: d\n    image: example.io/d:v1\n"), "", "outside the suite's table: example.io/d:v1"},
		{"a container outside the table", pod("  containers:\n  - name: c\n    image: example.io/c:v1\n"), "", "outside the suite's table: example.io/c:v1"},
		{"a comment after the image", pod("  containers:\n  - name: c\n    image: example.io/a:v1 # c\n"), "", "without a digest: example.io/a:v1"},
		{"a flow mapping", pod("  containers:\n  - {name: c, image: example.io/a:v1}\n"), "", "without a digest: example.io/a:v1"},
		{"an init container", pod("  initContainers:\n  - {name: i, image: example.io/a:v1}\n  containers:\n  - name: c\n    image: example.io/a:v1\n"), "", "without a digest: example.io/a:v1"},
		{"a Pod template of a CronJob", "apiVersion: batch/v1\nkind: CronJob\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n          - {name: c, image: example.io/a:v1}\n", "", "without a digest: example.io/a:v1"},
		{"a second document", pod("  containers:\n  - name: c\n    image: example.io/a:v1\n") + "---\n" + pod("  containers:\n  - {name: c, image: example.io/b:v1}\n"), "", "without a digest: example.io/b:v1"},
		{"a schema that names containers", "kind: CustomResourceDefinition\nspec:\n  properties:\n    containers:\n      type: array\n    initContainers:\n      type: array\n", "kind: CustomResourceDefinition\nspec:\n  properties:\n    containers:\n      type: array\n    initContainers:\n      type: array\n", ""},
		{"a document of comments", "# nothing\n---\n# nothing\n", "# nothing\n---\n# nothing\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := requireDigests([]byte(c.manifest), digests)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want an error with %q, got %v", c.wantErr, err)
			case c.wantErr == "" && string(got) != c.want:
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
