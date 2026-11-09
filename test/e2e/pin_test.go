// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const lodiaAPI = "go.lodia.io/lodia/api"

var (
	lodiaCommit = regexp.MustCompile(`(?m)^LODIA_COMMIT\s*[:?]?=\s*(\S+)\s*$`)
	imageLine   = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]+)?image:[ \t]+)"?([^"\s]+)"?[ \t]*$`)
)

func pinImages(manifest []byte, digests map[string]string) ([]byte, []string) {
	var unpinned []string
	pinned := imageLine.ReplaceAllFunc(manifest, func(line []byte) []byte {
		match := imageLine.FindSubmatch(line)
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

func lodiaPin(gomod, makefile []byte) error {
	file, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return err
	}
	rev := ""
	for _, require := range file.Require {
		if require.Mod.Path != lodiaAPI {
			continue
		}
		if rev, err = module.PseudoVersionRev(require.Mod.Version); err != nil {
			return fmt.Errorf("%s %s: %w", lodiaAPI, require.Mod.Version, err)
		}
	}
	if rev == "" {
		return fmt.Errorf("go.mod requires no %s", lodiaAPI)
	}
	found := lodiaCommit.FindSubmatch(makefile)
	if found == nil {
		return errors.New("the Makefile sets no LODIA_COMMIT")
	}
	if commit := string(found[1]); len(commit) != 40 || !strings.HasPrefix(commit, rev) {
		return fmt.Errorf("LODIA_COMMIT %q is not the full commit that go.mod pins, %s", commit, rev)
	}
	return nil
}

func TestLodiaCommitIsThePinnedAPI(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := lodiaPin(gomod, makefile); err != nil {
		t.Fatal(err)
	}

	const pinned = "require go.lodia.io/lodia/api v0.0.0-20261109145222-57375832e7fe\n"
	cases := []struct {
		name, gomod, makefile, want string
	}{
		{"no requirement", "module m\n", "LODIA_COMMIT = 57375832e7fe8b3a06374613797eb4c544fe7612\n", "requires no"},
		{"not a pseudo-version", "module m\nrequire go.lodia.io/lodia/api v1.0.0\n", "LODIA_COMMIT = 57375832e7fe8b3a06374613797eb4c544fe7612\n", "pseudo-version"},
		{"no commit", "module m\n" + pinned, "E2E_DIR = x\n", "sets no LODIA_COMMIT"},
		{"another commit", "module m\n" + pinned, "LODIA_COMMIT = 5737583aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "not the full commit"},
		{"a short commit", "module m\n" + pinned, "LODIA_COMMIT = 57375832e7fe\n", "not the full commit"},
		{"the pinned commit", "module m\n" + pinned, "LODIA_COMMIT = 57375832e7fe8b3a06374613797eb4c544fe7612\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := lodiaPin([]byte(c.gomod), []byte(c.makefile))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
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
