// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"
)

const (
	label      = "    cluster.x-k8s.io/provider: control-plane-x\n"
	namespace  = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns\n  labels:\n" + label
	deployment = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\n  namespace: ns\n  labels:\n" + label + "spec:\n  template:\n    spec:\n      serviceAccountName: manager\n      containers:\n      - name: manager\n"
	crd        = "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: c.x\n  labels:\n" + label
	role       = "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n  namespace: ns\n  labels:\n" + label + "rules:\n"
	binding    = "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: b\n  namespace: ns\n  labels:\n" + label +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: r\nsubjects:\n- kind: ServiceAccount\n  name: manager\n  namespace: ns\n"

	leases       = "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [create, get, update]\n"
	scopedLeases = "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [create]\n- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  resourceNames: [x]\n  verbs: [get, update]\n"
	everything   = "- apiGroups: ['*']\n  resources: ['*']\n  verbs: ['*']\n"
	events       = "- apiGroups: ['']\n  resources: [events]\n  verbs: [create, patch]\n"
	anyKind      = "apiVersion: v1\nkind: %s\nmetadata:\n  name: obj\n  labels:\n" + label
)

func manifest(docs ...string) string {
	return strings.Join(docs, "---\n")
}

func TestTheRenderedManifestsMeetClusterctl(t *testing.T) {
	grant := role + leases
	ok := manifest(namespace, crd, deployment, grant, binding)
	cases := []struct {
		name, manifest, want string
	}{
		{"a namespace, a manager and labels", ok, ""},
		{"comments and empty documents between", "# top\n---\n" + namespace + "---\n---\n# nothing\n---\n" + manifest(deployment, grant, binding), ""},
		{"no namespace", manifest(crd, deployment, grant, binding), "renders 0 Namespaces"},
		{"two namespaces", manifest(namespace, strings.Replace(namespace, "name: ns", "name: other", 1), deployment, grant, binding), "renders 2 Namespaces"},
		{"a container of another name", manifest(namespace, strings.Replace(deployment, "name: manager", "name: controller", 1)), "no Deployment with a container named manager"},
		{"a manager outside a Deployment", manifest(namespace, strings.Replace(deployment, "kind: Deployment", "kind: StatefulSet", 1)), "no Deployment with a container named manager"},
		{"a manager as the pod's service account", manifest(namespace, strings.Replace(deployment, "- name: manager", "- name: c", 1)), "no Deployment with a container named manager"},
		{"a Deployment without the label", manifest(namespace, strings.Replace(deployment, label, "    other: y\n", 1), grant, binding), "Deployment d has no label cluster.x-k8s.io/provider"},
		{"a CRD without the label", manifest(ok, strings.Replace(crd, "  labels:\n"+label, "", 1)), "CustomResourceDefinition c.x has no label"},
		{"the label only on a Pod template", manifest(namespace, strings.Replace(deployment, "  labels:\n"+label+"spec:\n  template:\n", "spec:\n  template:\n    metadata:\n      labels:\n    "+label, 1), grant, binding), "Deployment d has no label"},
		{"nothing at all", "", "renders 0 Namespaces"},
		{"text that is not YAML", "a: [\n", "yaml"},
	}
	for _, kind := range []string{
		"Node", "PersistentVolume", "PodSecurityPolicy", "CertificateSigningRequest", "ClusterRoleBinding", "ClusterRole", "VolumeAttachment", "StorageClass",
		"CSIDriver", "CSINode", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "CustomResourceDefinition", "PriorityClass", "RuntimeClass",
	} {
		cases = append(cases, struct{ name, manifest, want string }{kind + " outside every namespace", manifest(ok, fmt.Sprintf(anyKind, kind)), ""})
	}
	cases = append(cases, []struct{ name, manifest, want string }{
		{"a Role in another namespace", manifest(namespace, deployment, strings.Replace(role, "namespace: ns", "namespace: kube-system", 1)+leases, binding), `Role r is in the namespace "kube-system", not in the Namespace ns`},
		{"a Role in another namespace grants nothing", manifest(namespace, deployment, strings.Replace(role, "namespace: ns", "namespace: kube-system", 1)+leases, binding), "has no RoleBinding in ns to a Role that grants"},
		{"a RoleBinding in another namespace binds nothing", manifest(namespace, deployment, grant, strings.Replace(binding, "  name: b\n  namespace: ns", "  name: b\n  namespace: kube-system", 1)), "has no RoleBinding in ns to a Role that grants"},
		{"a Deployment without a namespace", manifest(namespace, strings.Replace(deployment, "  namespace: ns\n", "", 1), grant, binding), `Deployment d is in the namespace ""`},
		{"a namespaced kind clusterctl does not list", manifest(ok, fmt.Sprintf(anyKind, "Service")), `Service obj is in the namespace ""`},
		{"no RoleBinding", manifest(namespace, deployment, grant), "has no RoleBinding in ns to a Role that grants"},
		{"a RoleBinding to a Role that is not rendered", manifest(namespace, deployment, grant, strings.Replace(binding, "  name: r\nsubjects", "  name: leader-election\nsubjects", 1)), "has no RoleBinding in ns to a Role that grants"},
		{"a RoleBinding to a ClusterRole", manifest(namespace, deployment, grant, strings.Replace(binding, "kind: Role\n", "kind: ClusterRole\n", 1)), "has no RoleBinding in ns to a Role that grants"},
		{"a subject that is not the Deployment's ServiceAccount", manifest(namespace, deployment, grant, strings.Replace(binding, "- kind: ServiceAccount\n  name: manager", "- kind: ServiceAccount\n  name: controller-manager", 1)), `the ServiceAccount "manager" of Deployment d has no RoleBinding`},
		{"a subject in another namespace", manifest(namespace, deployment, grant, strings.Replace(binding, "  name: manager\n  namespace: ns", "  name: manager\n  namespace: other", 1)), "has no RoleBinding in ns to a Role that grants"},
		{"a subject that is not a ServiceAccount", manifest(namespace, deployment, grant, strings.Replace(binding, "- kind: ServiceAccount", "- kind: User", 1)), "has no RoleBinding in ns to a Role that grants"},
		{"a Role of events", manifest(namespace, deployment, role+events, binding), "has no RoleBinding in ns to a Role that grants"},
		{"create on events and the rest on leases", manifest(namespace, deployment, role+events+strings.Replace(leases, "create, ", "", 1), binding), "has no RoleBinding in ns to a Role that grants"},
		{"a Role that cannot be read", manifest(namespace, deployment, role+"  not: rules\n", binding), "decode Role r"},
		{"a RoleBinding that cannot be read", manifest(namespace, deployment, grant, strings.Replace(binding, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: r\n", "roleRef: x\n", 1)), "decode RoleBinding b"},
		{"another resource of the API group", manifest(namespace, deployment, role+strings.Replace(leases, "[leases]", "[leasecandidates]", 1), binding), "has no RoleBinding in ns to a Role that grants"},
		{"leases of another API group", manifest(namespace, deployment, role+strings.Replace(leases, "coordination.k8s.io", "example.io", 1), binding), "has no RoleBinding in ns to a Role that grants"},
		{"a Role that cannot create", manifest(namespace, deployment, role+strings.Replace(leases, "create, ", "", 1), binding), "has no RoleBinding in ns to a Role that grants"},
		{"a Role that cannot get", manifest(namespace, deployment, role+strings.Replace(leases, "get, ", "", 1), binding), "has no RoleBinding in ns to a Role that grants"},
		{"a Role that cannot update", manifest(namespace, deployment, role+strings.Replace(leases, ", update", "", 1), binding), "has no RoleBinding in ns to a Role that grants"},
		{"the grant in two rules", manifest(namespace, deployment, role+scopedLeases, binding), ""},
		{"the grant by wildcards", manifest(namespace, deployment, role+everything, binding), ""},
		{"the grant beside other rules", manifest(namespace, deployment, role+events+leases, binding), ""},
		{"a second manager without a grant", manifest(namespace, deployment, strings.Replace(strings.Replace(deployment, "name: d\n", "name: e\n", 1), "serviceAccountName: manager", "serviceAccountName: other", 1), grant, binding), `the ServiceAccount "other" of Deployment e has no RoleBinding`},
	}...)
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
