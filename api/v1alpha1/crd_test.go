// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.lodia.io/lodia/api/crds"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

var bases = os.DirFS(filepath.Join("..", "..", "config", "crd", "bases"))

func crd(t *testing.T, fsys fs.FS, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	out := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(raw, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func specOf(c *apiextensionsv1.CustomResourceDefinition) apiextensionsv1.JSONSchemaProps {
	return c.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
}

func TestTheSpecPassesLodiasSchemaThrough(t *testing.T) {
	ours := specOf(crd(t, bases, "controlplane.lodia.io_lodiacontrolplanes.yaml"))
	lodias := specOf(crd(t, crds.FS, "lodia.io_controlplanes.yaml"))
	ignore := []cmp.Option{
		cmpopts.IgnoreFields(apiextensionsv1.JSONSchemaProps{}, "Description"),
		cmpopts.SortSlices(func(a, b apiextensionsv1.ValidationRule) bool { return a.Rule < b.Rule }),
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
	}

	for name, want := range lodias.Properties {
		if name == "network" || name == "paused" {
			continue
		}
		if diff := cmp.Diff(want, ours.Properties[name], ignore...); diff != "" {
			t.Errorf("spec.%s (-Lodia +ours):\n%s", name, diff)
		}
	}
	for name := range ours.Properties {
		if _, ok := lodias.Properties[name]; !ok && name != "controlPlaneEndpoint" {
			t.Errorf("spec.%s is not Lodia's", name)
		}
	}
	required := slices.DeleteFunc(slices.Clone(lodias.Required), func(name string) bool { return name == "network" })
	if diff := cmp.Diff(required, ours.Required, ignore...); diff != "" {
		t.Errorf("spec required (-Lodia +ours):\n%s", diff)
	}
	if diff := cmp.Diff(lodias.XValidations, ours.XValidations, ignore...); diff != "" {
		t.Errorf("spec rules (-Lodia +ours):\n%s", diff)
	}
}

func TestTheCRDsCarryTheContractLabel(t *testing.T) {
	files, err := fs.Glob(bases, "*.yaml")
	if err != nil || len(files) != 2 {
		t.Fatalf("CRDs %v: %v", files, err)
	}
	for _, f := range files {
		if v := crd(t, bases, f).Labels["cluster.x-k8s.io/v1beta2"]; v != "v1alpha1" {
			t.Errorf("%s: cluster.x-k8s.io/v1beta2 is %q", f, v)
		}
	}
}
