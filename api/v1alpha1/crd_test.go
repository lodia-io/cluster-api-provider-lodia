// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func templateSpecOf(c *apiextensionsv1.CustomResourceDefinition) apiextensionsv1.JSONSchemaProps {
	return c.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["template"].Properties["spec"]
}

var ignore = []cmp.Option{
	cmpopts.IgnoreFields(apiextensionsv1.JSONSchemaProps{}, "Description"),
	cmpopts.SortSlices(func(a, b apiextensionsv1.ValidationRule) bool { return a.Rule < b.Rule }),
	cmpopts.SortSlices(func(a, b string) bool { return a < b }),
}

func TestTheSpecPassesLodiasSchemaThrough(t *testing.T) {
	ours := specOf(crd(t, bases, "controlplane.lodia.io_lodiacontrolplanes.yaml"))
	lodias := specOf(crd(t, crds.FS, "lodia.io_controlplanes.yaml"))

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
	shared := slices.DeleteFunc(slices.Clone(lodias.XValidations), func(rule apiextensionsv1.ValidationRule) bool {
		return strings.Contains(rule.Rule, "self.network")
	})
	if diff := cmp.Diff(shared, ours.XValidations, ignore...); diff != "" {
		t.Errorf("spec rules (-Lodia +ours):\n%s", diff)
	}
}

func TestTheSpecHasNeitherTheNetworkNorLodiasPause(t *testing.T) {
	ours := specOf(crd(t, bases, "controlplane.lodia.io_lodiacontrolplanes.yaml"))
	for _, name := range []string{"network", "paused"} {
		if _, ok := ours.Properties[name]; ok {
			t.Errorf("spec.%s is in the CRD: the network comes from the Cluster, and Lodia's pause stays an operator's break-glass switch", name)
		}
	}
}

func TestTheTemplateIsTheSpecWithoutWhatTheClusterSets(t *testing.T) {
	ours := specOf(crd(t, bases, "controlplane.lodia.io_lodiacontrolplanes.yaml"))
	template := templateSpecOf(crd(t, bases, "controlplane.lodia.io_lodiacontrolplanetemplates.yaml"))

	for _, name := range []string{"version", "controlPlaneEndpoint", "restoreFrom"} {
		delete(ours.Properties, name)
	}
	if diff := cmp.Diff(ours.Properties, template.Properties, ignore...); diff != "" {
		t.Errorf("template spec properties (-spec +template):\n%s", diff)
	}
	required := slices.DeleteFunc(slices.Clone(ours.Required), func(name string) bool { return name == "version" })
	if diff := cmp.Diff(required, template.Required, ignore...); diff != "" {
		t.Errorf("template spec required (-spec +template):\n%s", diff)
	}
	rules := slices.DeleteFunc(slices.Clone(ours.XValidations), func(rule apiextensionsv1.ValidationRule) bool {
		return strings.Contains(rule.Rule, "restoreFrom")
	})
	if diff := cmp.Diff(rules, template.XValidations, ignore...); diff != "" {
		t.Errorf("template spec rules (-spec +template):\n%s", diff)
	}
}

func TestTheCRDsCarryTheContractLabel(t *testing.T) {
	files, err := fs.Glob(bases, "*.yaml")
	if err != nil || len(files) != 2 {
		t.Fatalf("CRDs %v: %v", files, err)
	}
	for _, f := range files {
		labels := crd(t, bases, f).Labels
		if v := labels["cluster.x-k8s.io/v1beta2"]; v != "v1alpha1" {
			t.Errorf("%s: cluster.x-k8s.io/v1beta2 is %q", f, v)
		}
		if _, ok := labels["clusterctl.cluster.x-k8s.io"]; !ok {
			t.Errorf("%s: no clusterctl.cluster.x-k8s.io label, labels %v", f, labels)
		}
	}
}
