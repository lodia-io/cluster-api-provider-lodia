// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const release = "v0.1.0-alpha.2"

func clusterRoleOf(name string) string {
	return "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: " + name + "\n  labels:\n" + label + "rules: []\n"
}

func clusterBindingOf(name, role string) string {
	return bindingOf("ClusterRoleBinding", name, "ClusterRole", role, account("manager", "ns"))
}

func crdOf(name string, versions ...string) string {
	doc := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\n  labels:\n" + label + "spec:\n  versions:\n"
	for _, v := range versions {
		name, notServed := strings.CutSuffix(v, "!")
		doc += "  - name: " + name + "\n    served: " + map[bool]string{false: "true", true: "false"}[notServed] + "\n"
	}
	return doc
}

func upgradeFrom(previous, tree string) error {
	return upgrade(context.Background(), strings.NewReader(tree), release, func(context.Context) ([]byte, error) {
		return []byte(previous), nil
	})
}

func TestAnUpgradeThatClusterctlCannotCarryIsRefused(t *testing.T) {
	webhook := strings.Replace(anyKind("ValidatingWebhookConfiguration"), "name: obj", "name: hook", 1)
	previous := manifest(namespace, crdOf("c.x", "v1"), deployment, grant, binding, clusterRoleOf("cr"), clusterBindingOf("cb", "cr"), webhook)
	cases := []struct {
		name, previous, tree string
		want                 []string
	}{
		{"the same objects", previous, previous, nil},
		{"objects that the previous release lacks", previous, manifest(previous, clusterRoleOf("added"), clusterBindingOf("added", "added"), crdOf("d.x", "v1")), nil},
		{"a ClusterRole that the render drops", previous, manifest(namespace, crdOf("c.x", "v1"), deployment, clusterBindingOf("cb", "cr"), webhook), []string{"ClusterRole cr of " + release + " is not in the render"}},
		{"a ClusterRole that the render renames", previous, strings.Replace(previous, "kind: ClusterRole\nmetadata:\n  name: cr\n", "kind: ClusterRole\nmetadata:\n  name: renamed\n", 1), []string{"ClusterRole cr of " + release + " is not in the render"}},
		{"a ClusterRoleBinding that the render drops", previous, manifest(namespace, crdOf("c.x", "v1"), deployment, clusterRoleOf("cr"), webhook), []string{"ClusterRoleBinding cb of " + release + " is not in the render"}},
		{"a ClusterRoleBinding that the render renames", previous, strings.Replace(previous, "name: cb\n", "name: renamed\n", 1), []string{"ClusterRoleBinding cb of " + release + " is not in the render"}},
		{"a Namespace that the render renames", previous, strings.Replace(previous, "name: ns\n", "name: renamed\n", 1), []string{"Namespace ns of " + release + " is not in the render"}},
		{"a CRD that the render drops", previous, manifest(namespace, deployment, clusterRoleOf("cr"), clusterBindingOf("cb", "cr"), webhook), []string{"CustomResourceDefinition c.x of " + release + " is not in the render"}},
		{"a CRD whose name starts with the namespace and a dash, which clusterctl keeps", manifest(previous, crdOf("ns-d.x", "v1")), previous, []string{"CustomResourceDefinition ns-d.x of " + release + " is not in the render"}},
		{"a ClusterRole whose name starts with the namespace and a dash, which clusterctl deletes", manifest(previous, clusterRoleOf("ns-gone")), previous, nil},
		{"a ClusterRole whose name starts with the namespace and no dash, which clusterctl keeps", manifest(previous, clusterRoleOf("nsgone")), previous, []string{"ClusterRole nsgone of " + release + " is not in the render"}},
		{"a webhook configuration, which clusterctl deletes", previous, strings.Replace(previous, "name: hook\n", "name: renamed\n", 1), nil},
		{"a namespaced object that the render drops", previous, manifest(namespace, crdOf("c.x", "v1"), clusterRoleOf("cr"), clusterBindingOf("cb", "cr"), webhook), nil},
		{"the roleRef of a ClusterRoleBinding that names another ClusterRole", manifest(previous, clusterRoleOf("other")), manifest(strings.Replace(previous, "kind: ClusterRole\n  name: cr\n", "kind: ClusterRole\n  name: other\n", 1), clusterRoleOf("other")), []string{"ClusterRoleBinding cb has the roleRef", "is immutable"}},
		{"the roleRef of a ClusterRoleBinding that names a Role", previous, strings.Replace(previous, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cr\n", "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: cr\n", 1), []string{"ClusterRoleBinding cb has the roleRef"}},
		{"the roleRef of a ClusterRoleBinding in another group", previous, strings.Replace(previous, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cr\n", "roleRef:\n  apiGroup: other.example.test\n  kind: ClusterRole\n  name: cr\n", 1), []string{"ClusterRoleBinding cb has the roleRef"}},
		{"the subjects of a ClusterRoleBinding", previous, strings.Replace(previous, "- kind: ServiceAccount\n  name: manager\n  namespace: ns\n", "- kind: ServiceAccount\n  name: another\n  namespace: ns\n", 1), nil},
		{"the roleRef of a ClusterRoleBinding whose name starts with the namespace and a dash, which clusterctl deletes", manifest(previous, clusterBindingOf("ns-cb", "cr")), manifest(previous, clusterBindingOf("ns-cb", "another")), nil},
		{"the roleRef of a RoleBinding", previous, strings.Replace(previous, "kind: Role\n  name: r\n", "kind: Role\n  name: another\n", 1), nil},
		{"a CRD version that the render drops", manifest(namespace, crdOf("c.x", "v1", "v2")), manifest(namespace, crdOf("c.x", "v2")), []string{"CustomResourceDefinition c.x serves v1 in " + release + " and not in the render"}},
		{"a CRD version that the render renames", manifest(namespace, crdOf("c.x", "v1")), manifest(namespace, crdOf("c.x", "v1beta1")), []string{"CustomResourceDefinition c.x serves v1 in "}},
		{"every CRD version that the render drops", manifest(namespace, crdOf("c.x", "v1", "v2", "v3")), manifest(namespace, crdOf("c.x", "v3")), []string{"serves v1, v2 in " + release}},
		{"a CRD version that the render adds", manifest(namespace, crdOf("c.x", "v1")), manifest(namespace, crdOf("c.x", "v1", "v2")), nil},
		{"a CRD version that the previous release did not serve", manifest(namespace, crdOf("c.x", "v1", "v0!")), manifest(namespace, crdOf("c.x", "v1")), nil},
		{"a CRD version that the render no longer serves", manifest(namespace, crdOf("c.x", "v1", "v2")), manifest(namespace, crdOf("c.x", "v1", "v2!")), nil},
		{"a CRD of the render that has no versions", manifest(namespace, crdOf("c.x", "v1")), manifest(namespace, crdOf("c.x")), []string{"CustomResourceDefinition c.x serves v1 in "}},
		{"every problem at once", previous, manifest(namespace, crdOf("c.x", "v2"), clusterBindingOf("cb", "another")), []string{"ClusterRole cr of ", "ClusterRoleBinding cb has the roleRef", "CustomResourceDefinition c.x serves v1 in "}},
		{"components without a Namespace", manifest(clusterRoleOf("cr")), manifest(namespace, clusterRoleOf("cr")), []string{"hold 0 Namespaces"}},
		{"components with two Namespaces", manifest(namespace, strings.Replace(namespace, "name: ns", "name: other", 1)), namespace, []string{"hold 2 Namespaces"}},
		{"text that is not YAML", "a: [\n", previous, []string{"the components of " + release}},
		{"a render that is not YAML", previous, "a: [\n", []string{"yaml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := upgradeFrom(c.previous, c.tree)
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			for _, want := range c.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("want an error with %q, got %v", want, err)
				}
			}
		})
	}
}

func TestTheRenderIsComparedWithTheReleaseAndNotTheOtherWayAround(t *testing.T) {
	older := manifest(namespace, clusterRoleOf("old"))
	newer := manifest(namespace, clusterRoleOf("new"))
	if err := upgradeFrom(older, newer); err == nil || !strings.Contains(err.Error(), "ClusterRole old of "+release) {
		t.Errorf("a ClusterRole of the release that the render lacks: got %v", err)
	}
	if err := upgradeFrom(newer, older); err == nil || !strings.Contains(err.Error(), "ClusterRole new of "+release) {
		t.Errorf("the other way: got %v", err)
	}
	if err := upgradeFrom(namespace, newer); err != nil {
		t.Errorf("a ClusterRole that only the render has: got %v", err)
	}
}

func TestAnUpgradeFailsWhenTheComponentsCannotBeRead(t *testing.T) {
	unreachable := errors.New("the release is unreachable")
	err := upgrade(context.Background(), strings.NewReader(namespace), release, func(context.Context) ([]byte, error) { return nil, unreachable })
	if !errors.Is(err, unreachable) {
		t.Errorf("want the error of the download, got %v", err)
	}
}

func TestTheCommandRefusesArgumentsItDoesNotKnow(t *testing.T) {
	for _, args := range [][]string{{"upgrade"}, {"upgrade", "a", "b"}, {"check"}, {"a"}} {
		if err := run(args, strings.NewReader(namespace)); err == nil || !strings.Contains(err.Error(), "usage: components [upgrade DIR]") {
			t.Errorf("%v: want the usage, got %v", args, err)
		}
	}
}

func TestThePinsComponentsAreWhatTheUpgradeReads(t *testing.T) {
	dir := t.TempDir()
	name, components, err := fromThePin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "v") {
		t.Errorf("the release is named %q", name)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := components(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a download with a context that has ended: got %v", err)
	}
}
