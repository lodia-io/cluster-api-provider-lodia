// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

const (
	namespace  = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns\n  labels:\n    cluster.x-k8s.io/provider: control-plane-x\n"
	deployment = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\n  labels:\n    cluster.x-k8s.io/provider: control-plane-x\nspec:\n  template:\n    spec:\n      containers:\n      - name: manager\n"
	crd        = "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: c.x\n  labels:\n    cluster.x-k8s.io/provider: control-plane-x\n"
)

func TestTheRenderedManifestsMeetClusterctl(t *testing.T) {
	cases := []struct {
		name, manifest, want string
	}{
		{"a namespace, a manager and labels", namespace + "---\n" + crd + "---\n" + deployment, ""},
		{"comments and empty documents between", "# top\n---\n" + namespace + "---\n---\n# nothing\n---\n" + deployment, ""},
		{"no namespace", crd + "---\n" + deployment, "renders 0 Namespaces"},
		{"two namespaces", namespace + "---\n" + strings.Replace(namespace, "name: ns", "name: other", 1) + "---\n" + deployment, "renders 2 Namespaces"},
		{"a container of another name", namespace + "---\n" + strings.Replace(deployment, "name: manager", "name: controller", 1), "no Deployment with a container named manager"},
		{"a manager outside a Deployment", namespace + "---\n" + strings.Replace(deployment, "kind: Deployment", "kind: StatefulSet", 1), "no Deployment with a container named manager"},
		{"a manager as the pod's name", namespace + "---\n" + strings.Replace(deployment, "- name: manager", "- name: c\n      serviceAccountName: manager", 1), "no Deployment with a container named manager"},
		{"a Deployment without the label", namespace + "---\n" + strings.Replace(deployment, "    cluster.x-k8s.io/provider: control-plane-x\n", "    other: y\n", 1), "Deployment d has no label cluster.x-k8s.io/provider"},
		{"a CRD without the label", namespace + "---\n" + deployment + "---\n" + strings.Replace(crd, "  labels:\n    cluster.x-k8s.io/provider: control-plane-x\n", "", 1), "CustomResourceDefinition c.x has no label"},
		{"the label only on a Pod template", namespace + "---\n" + strings.Replace(deployment, "  labels:\n    cluster.x-k8s.io/provider: control-plane-x\nspec:\n  template:\n", "spec:\n  template:\n    metadata:\n      labels:\n        cluster.x-k8s.io/provider: control-plane-x\n", 1), "Deployment d has no label"},
		{"nothing at all", "", "renders 0 Namespaces"},
		{"text that is not YAML", "a: [\n", "yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := check(strings.NewReader(c.manifest))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
