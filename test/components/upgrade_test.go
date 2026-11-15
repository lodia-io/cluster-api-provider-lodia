// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lodia.io/cluster-api-provider-lodia/test/pin"
)

const release = "v0.1.0-alpha.2"

func clusterRoleOf(name string) string {
	return "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: " + name + "\n  labels:\n" + label + "rules: []\n"
}

func clusterBindingOf(name, role string) string {
	return bindingOf("ClusterRoleBinding", name, "ClusterRole", role, account("manager", "ns"))
}

func crdOf(name string, versions ...string) string {
	_, group, _ := strings.Cut(name, ".")
	doc := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\n  labels:\n" + label + "spec:\n  group: " + group + "\n  versions:\n"
	for _, v := range versions {
		v, notServed := strings.CutSuffix(v, "!")
		v, storage := strings.CutSuffix(v, "*")
		doc += "  - name: " + v + "\n    served: " + map[bool]string{false: "true", true: "false"}[notServed] + "\n    storage: " + map[bool]string{false: "false", true: "true"}[storage] + "\n"
	}
	return doc
}

func customResourceOf(group, kind, name string) string {
	return "apiVersion: " + group + "/v1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n  namespace: ns\n  labels:\n" + label
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
		{"a Namespace that the render renames, which clusterctl installs into the inventory's namespace instead", previous, strings.Replace(previous, "name: ns\n", "name: renamed\n", 1), nil},
		{"a Namespace that the render drops", previous, manifest(crdOf("c.x", "v1"), deployment, grant, binding, clusterRoleOf("cr"), clusterBindingOf("cb", "cr"), webhook), nil},
		{"a CRD that the render drops", previous, manifest(namespace, deployment, clusterRoleOf("cr"), clusterBindingOf("cb", "cr"), webhook), []string{"CustomResourceDefinition c.x of " + release + " is not in the render"}},
		{"a CRD whose name starts with the namespace and a dash, which clusterctl keeps", manifest(previous, crdOf("ns-d.x", "v1")), previous, []string{"CustomResourceDefinition ns-d.x of " + release + " is not in the render"}},
		{"a ClusterRole whose name starts with the namespace and a dash, which clusterctl deletes", manifest(previous, clusterRoleOf("ns-gone")), previous, nil},
		{"a ClusterRole whose name starts with the namespace and no dash, which clusterctl keeps", manifest(previous, clusterRoleOf("nsgone")), previous, []string{"ClusterRole nsgone of " + release + " is not in the render"}},
		{"a webhook configuration, which clusterctl deletes", previous, strings.Replace(previous, "name: hook\n", "name: renamed\n", 1), nil},
		{"a namespaced object that the render drops", previous, manifest(namespace, crdOf("c.x", "v1"), clusterRoleOf("cr"), clusterBindingOf("cb", "cr"), webhook), nil},
		{"a custom resource of a CRD of the release that the render drops, which clusterctl does not list", manifest(previous, customResourceOf("x", "Thing", "t")), previous, []string{"Thing t of " + release + " is not in the render"}},
		{"a custom resource of a CRD of the release that the render keeps", manifest(previous, customResourceOf("x", "Thing", "t")), manifest(previous, customResourceOf("x", "Thing", "t")), nil},
		{"a custom resource of a CRD of the release that the render renames", manifest(previous, customResourceOf("x", "Thing", "t")), manifest(previous, customResourceOf("x", "Thing", "other")), []string{"Thing t of " + release + " is not in the render"}},
		{"a custom resource of Cluster API's group that the render drops", manifest(previous, customResourceOf("cluster.x-k8s.io", "ClusterClass", "cc")), previous, []string{"ClusterClass cc of " + release + " is not in the render"}},
		{"a custom resource of a group under Cluster API's that the render drops", manifest(previous, customResourceOf("infrastructure.cluster.x-k8s.io", "DockerClusterTemplate", "t")), previous, []string{"DockerClusterTemplate t of " + release + " is not in the render"}},
		{"a custom resource of a group that is only named like Cluster API's", manifest(previous, customResourceOf("notcluster.x-k8s.io", "Thing", "t")), previous, nil},
		{"a custom resource of a group that no CRD of the release defines, which clusterctl lists and deletes", manifest(previous, customResourceOf("cert-manager.io", "Certificate", "c")), previous, nil},
		{"a namespaced object of the core group when a CRD of the release names no group", manifest(namespace, crd, anyKind("ConfigMap")), manifest(namespace, crd), nil},
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
		{"a CRD version that the render no longer serves", manifest(namespace, crdOf("c.x", "v1", "v2")), manifest(namespace, crdOf("c.x", "v1", "v2!")), []string{"CustomResourceDefinition c.x serves v2 in " + release + " and not in the render"}},
		{"a CRD version that neither release serves", manifest(namespace, crdOf("c.x", "v1", "v2!")), manifest(namespace, crdOf("c.x", "v1", "v2!")), nil},
		{"a CRD version that the previous release did not serve and the render serves", manifest(namespace, crdOf("c.x", "v1", "v2!")), manifest(namespace, crdOf("c.x", "v1", "v2")), nil},
		{"the only version of a CRD, which is its storage version, that the render no longer serves", manifest(namespace, crdOf("c.x", "v1*")), manifest(namespace, crdOf("c.x", "v1*!")), []string{"CustomResourceDefinition c.x serves v1 in " + release + " and not in the render"}},
		{"a storage version that the render drops and the previous release did not serve", manifest(namespace, crdOf("c.x", "v1*!", "v2")), manifest(namespace, crdOf("c.x", "v2*")), []string{"CustomResourceDefinition c.x has the storage version v1 in " + release + " and the render of config/default does not list it", "status.storedVersions"}},
		{"a storage version that the render keeps, as the previous release did, unserved", manifest(namespace, crdOf("c.x", "v1*!", "v2")), manifest(namespace, crdOf("c.x", "v1*!", "v2")), nil},
		{"a storage version that the render moves to another version it lists", manifest(namespace, crdOf("c.x", "v1*", "v2")), manifest(namespace, crdOf("c.x", "v1", "v2*")), nil},
		{"a storage version that the render drops while it served it", manifest(namespace, crdOf("c.x", "v1*", "v2")), manifest(namespace, crdOf("c.x", "v2*")), []string{"serves v1 in " + release, "has the storage version v1 in " + release}},
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

func thePreviousRelease(components string, err error) previousRelease {
	return func(string) (string, func(context.Context) ([]byte, error), error) {
		return release, func(context.Context) ([]byte, error) { return []byte(components), nil }, err
	}
}

func TestTheCommandComparesTheRenderWithTheComponentsOfThePreviousRelease(t *testing.T) {
	var dir string
	previous := func(d string) (string, func(context.Context) ([]byte, error), error) {
		dir = d
		return thePreviousRelease(manifest(namespace, clusterRoleOf("cr")), nil)(d)
	}
	err := run([]string{"upgrade", "the-cache"}, strings.NewReader(namespace), previous)
	if err == nil || !strings.Contains(err.Error(), "ClusterRole cr of "+release+" is not in the render") {
		t.Errorf("a render that lacks a ClusterRole of the release: got %v", err)
	}
	if dir != "the-cache" {
		t.Errorf("the release was looked up in %q, want the directory of the arguments", dir)
	}
	if err := run([]string{"upgrade", "the-cache"}, strings.NewReader(manifest(namespace, clusterRoleOf("cr"))), previous); err != nil {
		t.Errorf("a render that has it: got %v", err)
	}
}

func TestTheCommandFailsWhenThePreviousReleaseCannotBeNamed(t *testing.T) {
	unnamed := errors.New("the pin is not a release")
	err := run([]string{"upgrade", "the-cache"}, strings.NewReader(namespace), thePreviousRelease("", unnamed))
	if !errors.Is(err, unnamed) {
		t.Errorf("want the error of the pin, got %v", err)
	}
}

func TestTheCommandWithNoArgumentsChecksTheRenderAlone(t *testing.T) {
	previous := func(string) (string, func(context.Context) ([]byte, error), error) {
		t.Error("the release was looked up")
		return "", nil, errors.New("unexpected")
	}
	if err := run(nil, strings.NewReader(namespace), previous); err == nil || !strings.Contains(err.Error(), "no Deployment with a container named manager") {
		t.Errorf("a render that holds no manager: got %v", err)
	}
}

func TestTheCommandRefusesArgumentsItDoesNotKnow(t *testing.T) {
	for _, args := range [][]string{{"upgrade"}, {"upgrade", "a", "b"}, {"check"}, {"a"}} {
		if err := run(args, strings.NewReader(namespace), thePreviousRelease("", nil)); err == nil || !strings.Contains(err.Error(), "usage: components [upgrade DIR]") {
			t.Errorf("%v: want the usage, got %v", args, err)
		}
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestThePinsComponentsAreWhatTheUpgradeReads(t *testing.T) {
	pinned, err := pin.Previous()
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	transport := http.DefaultTransport
	http.DefaultTransport = roundTripper(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("kind: Namespace\n")), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	dir := t.TempDir()
	name, components, err := fromThePin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if name != pinned.Version {
		t.Errorf("the release is named %q, the pin names %q", name, pinned.Version)
	}

	_, err = components(t.Context())

	if err == nil || !strings.Contains(err.Error(), pinned.ComponentsURL()) || !strings.Contains(err.Error(), "the pin is "+pinned.Components) {
		t.Errorf("a download that has another sum than the pin's: want the refusal that names the components' URL and sum, got %v", err)
	}
	if len(urls) != 1 || urls[0] != pinned.ComponentsURL() {
		t.Errorf("downloaded %v, want %s alone", urls, pinned.ComponentsURL())
	}
	if _, err := os.Stat(filepath.Join(dir, pinned.Version)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a download with the wrong sum was written to %s: %v", dir, err)
	}
}
