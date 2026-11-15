// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package build_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/api/crds"
)

func requireTheKustomizationToListEveryCRD(kustomization []byte, files []string) error {
	var parsed struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(kustomization, &parsed); err != nil {
		return err
	}
	listed := slices.Sorted(slices.Values(parsed.Resources))
	if want := slices.Sorted(slices.Values(files)); !slices.Equal(listed, want) {
		return fmt.Errorf("the Kustomization of api/crds lists %v and the directory holds %v: a CRD that it leaves out is missing from every install of config/default", listed, want)
	}
	return nil
}

func TestTheKustomizationOfTheCRDsListsEveryCRDOfTheAPI(t *testing.T) {
	files, err := fs.Glob(crds.FS, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("crds.FS holds no CRD")
	}
	kustomization, err := os.ReadFile(filepath.Join(root, "api", "crds", "Kustomization"))
	if err != nil {
		t.Fatal(err)
	}
	if err := requireTheKustomizationToListEveryCRD(kustomization, files); err != nil {
		t.Fatal(err)
	}
}

func TestAKustomizationThatDoesNotListEveryCRDIsRefused(t *testing.T) {
	files := []string{"a.yaml", "b.yaml"}
	const head = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"
	cases := []struct {
		name, kustomization, want string
	}{
		{"both, in the other order", head + "resources:\n- b.yaml\n- a.yaml\n", ""},
		{"one", head + "resources:\n- a.yaml\n", "lists [a.yaml]"},
		{"none", head, "lists []"},
		{"a file that the directory lacks", head + "resources:\n- a.yaml\n- b.yaml\n- c.yaml\n", "lists [a.yaml b.yaml c.yaml]"},
		{"one twice", head + "resources:\n- a.yaml\n- b.yaml\n- b.yaml\n", "lists [a.yaml b.yaml b.yaml]"},
		{"not YAML", "resources: [", "yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireTheKustomizationToListEveryCRD([]byte(c.kustomization), files)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
