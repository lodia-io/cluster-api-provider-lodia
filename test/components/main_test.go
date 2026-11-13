// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	utilyaml "sigs.k8s.io/cluster-api/util/yaml"
)

const (
	lease      = "controlplane.lodia.io"
	label      = "    cluster.x-k8s.io/provider: control-plane-x\n"
	namespace  = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns\n  labels:\n" + label
	deployment = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\n  namespace: ns\n  labels:\n" + label + "spec:\n  template:\n    spec:\n      serviceAccountName: manager\n      containers:\n      - name: manager\n        image: ko://example.invalid/manager\n"
	crd        = "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: c.x\n  labels:\n" + label
	role       = "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n  namespace: ns\n  labels:\n" + label + "rules:\n"
	binding    = "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: b\n  namespace: ns\n  labels:\n" + label +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: r\nsubjects:\n- kind: ServiceAccount\n  name: manager\n  namespace: ns\n"

	clusterRoleHead = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: cr\n  labels:\n" + label + "rules:\n"
	clusterBinding  = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: cb\n  labels:\n" + label +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cr\nsubjects:\n- kind: ServiceAccount\n  name: manager\n  namespace: ns\n"

	bootstrapRoleHead = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: bootstrap\n  labels:\n" + label + "rules:\n"
	bootstrapGet      = "- apiGroups: [controlplane.lodia.io]\n  resources: [lodiacontrolplanes]\n  verbs: [get]\n"
	bootstrapRole     = bootstrapRoleHead + bootstrapGet
	bootstrapBinding  = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: bootstrap\n  labels:\n" + label +
		"roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: bootstrap\nsubjects:\n" + bootstrapSubject
	bootstrapSubject = "- apiGroup: rbac.authorization.k8s.io\n  kind: User\n  name: 'system:serviceaccount:${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE:=capi-kubeadm-bootstrap-system}:${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=capi-kubeadm-bootstrap-manager}'\n"

	createLeases = "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [create]\n"
	named        = "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  resourceNames: [%s]\n  verbs: [%s]\n"
	everything   = "- apiGroups: ['*']\n  resources: ['*']\n  verbs: ['*']\n"
	events       = "- apiGroups: ['']\n  resources: [events]\n  verbs: [create, patch]\n"
	secrets      = "- apiGroups: ['']\n  resources: [secrets]\n  verbs: [get, create]\n"
	lodiaKinds   = "- apiGroups: [lodia.io]\n  resources: ['*']\n  verbs: [get, create]\n"
)

var (
	leaderElection = events + createLeases + fmt.Sprintf(named, lease, "get, update")
	grant          = role + leaderElection
	clusterRole    = clusterRoleHead + secrets + lodiaKinds
)

var outsideEveryNamespace = []string{
	"Node", "PersistentVolume", "PodSecurityPolicy", "CertificateSigningRequest", "ClusterRoleBinding", "ClusterRole", "VolumeAttachment", "StorageClass",
	"CSIDriver", "CSINode", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "CustomResourceDefinition", "PriorityClass", "RuntimeClass",
}

type renderCase struct{ name, manifest, want string }

var versionsClusterctlConverts = map[string]string{
	"ClusterRoleBinding":             "rbac.authorization.k8s.io/v1",
	"ValidatingWebhookConfiguration": "admissionregistration.k8s.io/v1",
	"MutatingWebhookConfiguration":   "admissionregistration.k8s.io/v1",
	"CustomResourceDefinition":       "apiextensions.k8s.io/v1",
}

func anyKind(kind string) string {
	version, ok := versionsClusterctlConverts[kind]
	if !ok {
		version = "v1"
	}
	return "apiVersion: " + version + "\nkind: " + kind + "\nmetadata:\n  name: obj\n  labels:\n" + label
}

func manifest(docs ...string) string {
	return strings.Join(docs, "---\n")
}

func rename(doc, from, to string) string {
	return strings.Replace(doc, "name: "+from+"\n", "name: "+to+"\n", 1)
}

func user(name string) string {
	return "- kind: User\n  name: '" + name + "'\n"
}

func group(name string) string {
	return "- kind: Group\n  name: '" + name + "'\n"
}

func account(name, ns string) string {
	return "- kind: ServiceAccount\n  name: " + name + "\n  namespace: " + ns + "\n"
}

func bindingOf(kind, name, refKind, ref string, subjects ...string) string {
	doc := "apiVersion: rbac.authorization.k8s.io/v1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n"
	if kind == "RoleBinding" {
		doc += "  namespace: ns\n"
	}
	return doc + "  labels:\n" + label + "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: " + refKind + "\n  name: " + ref + "\nsubjects:\n" + strings.Join(subjects, "")
}

func runCases(t *testing.T, cases []renderCase) {
	t.Helper()
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

func TestTheRenderedManifestsMeetClusterctl(t *testing.T) {
	ok := manifest(namespace, bootstrapRole, bootstrapBinding, crd, deployment, grant, binding, clusterRole, clusterBinding)
	cases := []renderCase{
		{"a namespace, a manager and labels", ok, ""},
		{"comments and empty documents between", "# top\n---\n" + namespace + "---\n---\n# nothing\n---\n" + manifest(bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, clusterBinding), ""},
		{"no namespace", manifest(crd, deployment, grant, binding), "renders 0 Namespaces"},
		{"two namespaces", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(namespace, "name: ns", "name: other", 1), deployment, grant, binding), "renders 2 Namespaces"},
		{"a container of another name", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, "name: manager", "name: controller", 1)), "no Deployment with a container named manager"},
		{"a manager outside a Deployment", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, "kind: Deployment", "kind: StatefulSet", 1)), "no Deployment with a container named manager"},
		{"a manager as the pod's service account", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, "- name: manager", "- name: c", 1)), "no Deployment with a container named manager"},
		{"a Deployment without the label", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, label, "    other: y\n", 1), grant, binding), "Deployment d has no label cluster.x-k8s.io/provider"},
		{"a CRD without the label", manifest(ok, strings.Replace(crd, "  labels:\n"+label, "", 1)), "CustomResourceDefinition c.x has no label"},
		{"the label only on a Pod template", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, "  labels:\n"+label+"spec:\n  template:\n", "spec:\n  template:\n    metadata:\n      labels:\n    "+label, 1), grant, binding), "Deployment d has no label"},
		{"nothing at all", "", "renders 0 Namespaces"},
		{"text that is not YAML", "a: [\n", "yaml"},
	}
	for _, kind := range outsideEveryNamespace {
		cases = append(cases, renderCase{kind + " outside every namespace", manifest(ok, anyKind(kind)), ""})
	}
	cases = append(cases, []renderCase{
		{"a Role in another namespace", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, strings.Replace(grant, "namespace: ns", "namespace: kube-system", 1), binding), `Role r is in the namespace "kube-system", not in the Namespace ns`},
		{"a RoleBinding in another namespace", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "  name: b\n  namespace: ns", "  name: b\n  namespace: kube-system", 1), clusterRole, clusterBinding), `RoleBinding b is in the namespace "kube-system", not in the Namespace ns`},
		{"a Deployment without a namespace", manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, "  namespace: ns\n", "", 1), grant, binding), `Deployment d is in the namespace ""`},
		{"a namespaced kind clusterctl does not list", manifest(ok, anyKind("Service")), `Service obj is in the namespace ""`},
	}...)
	runCases(t, cases)
}

func TestEveryVariableHasANonEmptyDefault(t *testing.T) {
	const missing = "has no default, or an empty one"
	ok := manifest(namespace, bootstrapRole, bootstrapBinding, crd, deployment, grant, binding, clusterRole, clusterBinding)
	with := func(docs ...string) string {
		return manifest(append([]string{namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, clusterBinding}, docs...)...)
	}
	rule := func(resource string) string {
		return manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRoleHead+secrets+"- apiGroups: [coordination.k8s.io]\n  resources: ['"+resource+"']\n  verbs: [get]\n", clusterBinding)
	}
	args := func(arg string) string {
		return manifest(namespace, bootstrapRole, bootstrapBinding, deployment+"        args:\n        - '"+arg+"'\n", grant, binding, clusterRole, clusterBinding)
	}
	subject := func(name string) string {
		return with(bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cluster-admin", group(name)))
	}
	kind := func(value string) string {
		return with(strings.Replace(bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cluster-admin", account("manager", "ns")), "kind: ClusterRoleBinding", "kind: "+value, 1))
	}
	cases := []renderCase{
		{"a variable without a default in a ClusterRole rule", rule("${X}"), "the variable X " + missing},
		{"a variable without a default in a subject", subject("${X}"), "the variable X " + missing},
		{"a variable without a default in a kind", kind("${X}"), "the variable X " + missing},
		{"a variable without a default in the container's args", args("${X}"), "the variable X " + missing},
		{"a variable without a default in a Namespace", strings.Replace(ok, "name: ns\n", "name: ${X}\n", 1), "the variable X " + missing},
		{"a variable with an empty default", args("${X:=}"), "the variable X " + missing},
		{"a variable spelled with spaces", args("${ X }"), "the variable X " + missing},
		{"a second variable without a default", args("${X:=a}${Y}"), "the variable Y " + missing},
		{"a variable first without a default and then with one", args("${X}-${X:=a}"), "the variable X " + missing},
		{"a default that is a variable", args("${X:=${Y}}"), "the default of the variable X references the variables [Y]"},
		{"a variable that is not closed", rule("${X"), "clusterctl cannot read the variables of config/default"},
		{"a variable with a default and then without", args("${X:=a}-${X}"), "referenced again without its default or with another one"},
		{"a variable with a default and then with another", args("${X:=a}-${X:=b}"), "referenced again without its default or with another one"},
		{"a variable with the same default twice", args("${X:=a}-${X:=a}"), ""},
		{"a variable with a default", args("${X:=a}"), ""},
		{"a variable with the other default form", args("${X:-a}"), ""},
		{"a variable with a default in a CustomResourceDefinition", strings.Replace(ok, "name: c.x", "name: ${X:=c.x}", 1), ""},
		{"a variable with a default in a Namespace", strings.Replace(ok, "name: ns\n", "name: ${X:=ns}\n", 1), "not in the Namespace ${X:=ns}"},
		{"an escaped dollar", args("$$X"), ""},
		{"an escaped variable", args("$${X}"), ""},
		{"a dollar before a name", rule("$X"), ""},
		{"a dollar before a parenthesis", rule("$(X)"), ""},
		{"a dollar alone", rule("$"), ""},
		{"a Kubernetes reference in the container's args", args("--id=$(POD_NAME)"), ""},
	}
	runCases(t, cases)
}

func TestEachRunIsChecked(t *testing.T) {
	ok := manifest(namespace, bootstrapRole, bootstrapBinding, crd, deployment, grant, binding, clusterRole, clusterBinding)
	rule := func(resource string) string {
		return manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRoleHead+secrets+"- apiGroups: [coordination.k8s.io]\n  resources: ['"+resource+"']\n  verbs: [get]\n", clusterBinding)
	}
	clusterAdmin := func(subject string) string {
		return manifest(ok, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cluster-admin", subject))
	}
	cases := []struct {
		name, manifest         string
		atDefaults, atExplicit bool
	}{
		{"nothing wrong", ok, false, false},
		{"a variable that no check reads", strings.Replace(ok, "name: c.x", "name: ${X:=c.x}", 1), false, false},
		{"a widened ClusterRole", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRoleHead+everything, clusterBinding), true, true},
		{"cluster-admin bound to the manager", clusterAdmin(account("manager", "ns")), true, true},
		{"a verb that is a variable's default", strings.Replace(ok, "verbs: [get, update]", "verbs: [get, update, '${X:=delete}']", 1), true, true},
		{"leases that are a variable's default", rule("${X:=leases}"), true, false},
		{"leases that are a variable's default in the other form", rule("${X:-leases}"), true, false},
		{"cluster-admin bound to a group that is a variable's default", clusterAdmin(group("${X:=system:serviceaccounts}")), true, true},
		{"the ClusterRole of the binding named by a variable's default", strings.Replace(ok, "  name: cr\nsubjects", "  name: ${X:=cr}\nsubjects", 1), false, true},
		{"the Role of the binding named by a variable's default", strings.Replace(ok, "kind: Role\n  name: r\n", "kind: Role\n  name: ${X:=r}\n", 1), false, true},
		{"the manager's ServiceAccount named by a variable's default", strings.Replace(ok, "serviceAccountName: manager", "serviceAccountName: ${X:=manager}", 1), false, true},
		{"the User of another namespace by default", strings.Replace(ok, "${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE:=capi-kubeadm-bootstrap-system}", "${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE:=other}", 1), true, false},
		{"the User of another name by default", strings.Replace(ok, "${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=capi-kubeadm-bootstrap-manager}", "${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=other}", 1), true, false},
		{"the User named without the variables", strings.Replace(ok, bootstrapSubject, "- apiGroup: rbac.authorization.k8s.io\n  kind: User\n  name: system:serviceaccount:capi-kubeadm-bootstrap-system:capi-kubeadm-bootstrap-manager\n", 1), false, true},
		{"the User named by the variables in each other's place", strings.Replace(ok, bootstrapSubject, "- apiGroup: rbac.authorization.k8s.io\n  kind: User\n  name: 'system:serviceaccount:${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=capi-kubeadm-bootstrap-manager}:${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE:=capi-kubeadm-bootstrap-system}'\n", 1), true, true},
		{"the User named by another variable for the name", strings.Replace(ok, "${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=", "${LODIA_OTHER_SA_NAME:=", 1), false, true},
		{"the verb of the User's ClusterRole that is a variable's default", strings.Replace(ok, "verbs: [get]\n", "verbs: ['${X:=get}']\n", 1), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := check(strings.NewReader(c.manifest))
			for run, want := range map[string]bool{defaultsRun: c.atDefaults, explicitRun: c.atExplicit} {
				if got := err != nil && strings.Contains(err.Error(), "with "+run+": "); got != want {
					t.Errorf("refused with %s: %v, want %v; the error is %v", run, got, want, err)
				}
			}
		})
	}
}

func TestTheRunsProcessWhatClusterctlInstalls(t *testing.T) {
	const pinned = "registry.example/manager@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	t.Setenv("X", "from-the-environment")
	render := func(image string) []byte {
		return []byte(manifest(namespace, bootstrapRole, bootstrapBinding, strings.Replace(deployment, "ko://example.invalid/manager", image, 1)+"        args:\n        - --x=${X:=default}\n"))
	}
	for _, c := range []struct {
		name, image string
		values      map[string]string
		wantImage   string
		wantArg     string
	}{
		{"a ko reference and no value", "ko://example.invalid/manager", nil, pinnedImage, "--x=default"},
		{"a ko reference and a value", "ko://example.invalid/manager", map[string]string{"X": "set"}, pinnedImage, "--x=set"},
		{"a digest reference and no value", pinned, nil, pinned, "--x=default"},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs, err := install(render(c.image), c.values)
			if err != nil {
				t.Fatal(err)
			}
			managers := managersOf(objs)
			if len(managers) != 1 {
				t.Fatalf("got %d managers, want 1", len(managers))
			}
			containers, _, _ := unstructured.NestedSlice(managers[0].Object, "spec", "template", "spec", "containers")
			container := containers[0].(map[string]any)
			if container["image"] != c.wantImage || !slices.Equal(container["args"].([]any), []any{c.wantArg}) {
				t.Errorf("got image %v and args %v, want %s and [%s]", container["image"], container["args"], c.wantImage, c.wantArg)
			}
		})
	}
}

func TestTheManagerReachesOnlyItsLeaderElectionRoleAndItsClusterRole(t *testing.T) {
	manager := account("manager", "ns")
	with := func(docs ...string) string {
		return manifest(append([]string{namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, clusterBinding}, docs...)...)
	}
	withRole := func(rules string) string {
		return manifest(namespace, bootstrapRole, bootstrapBinding, deployment, role+rules, binding, clusterRole, clusterBinding)
	}
	withClusterRole := func(rules string) string {
		return manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRoleHead+rules, clusterBinding)
	}
	namedLease := func(names, verbs string) string { return fmt.Sprintf(named, names, verbs) }
	role2, clusterRole2 := rename(grant, "r", "r2"), rename(clusterRole, "cr", "cr2")
	cases := []renderCase{
		{"the manager's two roles", with(), ""},
		{"no RoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, clusterRole, clusterBinding), "reaches the Roles [], want the leader election Role in ns alone"},
		{"a RoleBinding to a Role that is not rendered", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "  name: r\nsubjects", "  name: leader-election\nsubjects", 1), clusterRole, clusterBinding), "has a RoleBinding to Role ns/leader-election, which config/default does not render"},
		{"a RoleBinding to a ClusterRole", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "kind: Role\n", "kind: ClusterRole\n", 1), clusterRole, clusterBinding), `the ServiceAccount "manager" of Deployment d reaches the Roles [], want`},
		{"a subject that is not the Deployment's ServiceAccount", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "- kind: ServiceAccount\n  name: manager", "- kind: ServiceAccount\n  name: controller-manager", 1), clusterRole, clusterBinding), `the ServiceAccount "manager" of Deployment d reaches the Roles [], want`},
		{"a subject in another namespace is the manager's ServiceAccount once clusterctl rewrites it", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "  name: manager\n  namespace: ns", "  name: manager\n  namespace: other", 1), clusterRole, clusterBinding), ""},
		{"a subject that is not a ServiceAccount", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "- kind: ServiceAccount", "- kind: User", 1), clusterRole, clusterBinding), "reaches the Roles [], want"},
		{"other subjects between the manager's ServiceAccount in a RoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "subjects:\n", "subjects:\n"+user("u"), 1)+group("g"), clusterRole, clusterBinding), "RoleBinding b binds Role ns/r to [{Kind:User APIGroup: Name:u Namespace:} {Kind:ServiceAccount"},
		{"a Role that cannot be read", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, role+"  not: rules\n", binding, clusterRole, clusterBinding), "decode Role r"},
		{"a RoleBinding that cannot be read", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: r\n", "roleRef: x\n", 1), clusterRole, clusterBinding), "unable to convert unstructured object to rbac.authorization.k8s.io/v1, Kind=RoleBinding"},
		{"a ClusterRole that cannot be read", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRoleHead+"  not: rules\n", clusterBinding), "decode ClusterRole cr"},
		{"a ClusterRoleBinding that cannot be read", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cr\n", "roleRef: x\n", 1)), "unable to convert unstructured object to rbac.authorization.k8s.io/v1, Kind=ClusterRoleBinding"},

		{"no ClusterRoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole), "reaches the ClusterRoles [], want the manager's ClusterRole alone"},
		{"a ClusterRole bound by a RoleBinding only", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, bindingOf("RoleBinding", "b2", "ClusterRole", "cr", manager)), "has no ClusterRoleBinding to ClusterRole cr"},
		{"a ClusterRole bound to the manager by a RoleBinding and to another ServiceAccount by a ClusterRoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, bindingOf("RoleBinding", "b2", "ClusterRole", "cr", manager), bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr", account("other", "ns"))), "has no ClusterRoleBinding to ClusterRole cr"},
		{"a ClusterRole bound to the manager by a RoleBinding and by a ClusterRoleBinding of a kind of neither", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, bindingOf("RoleBinding", "b2", "ClusterRole", "cr", manager), bindingOf("ClusterRoleBinding", "cb2", "Foo", "cr", manager)), "has no ClusterRoleBinding to ClusterRole cr"},
		{"a ClusterRoleBinding to a ClusterRole that is not rendered", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "  name: cr\nsubjects", "  name: manager\nsubjects", 1)), "has a binding to ClusterRole manager, which config/default does not render"},
		{"a ClusterRoleBinding to a ClusterRole named like a Role", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, strings.Replace(clusterBinding, "  name: cr\nsubjects", "  name: r\nsubjects", 1)), "has a binding to ClusterRole r, which config/default does not render"},
		{"a ClusterRoleBinding to a Role", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "kind: ClusterRole\n", "kind: Role\n", 1)), "reaches the ClusterRoles [], want the manager's ClusterRole alone"},
		{"a ClusterRoleBinding subject that is not the Deployment's ServiceAccount", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "name: manager\n  namespace: ns", "name: other\n  namespace: ns", 1)), `the ServiceAccount "manager" of Deployment d reaches the ClusterRoles []`},
		{"a ClusterRoleBinding subject in another namespace is the manager's ServiceAccount once clusterctl rewrites it", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "name: manager\n  namespace: ns", "name: manager\n  namespace: other", 1)), ""},
		{"a ClusterRoleBinding subject that is not a ServiceAccount", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "- kind: ServiceAccount", "- kind: User", 1)), "reaches the ClusterRoles []"},
		{"other subjects between the manager's ServiceAccount in a ClusterRoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "subjects:\n", "subjects:\n"+user("u"), 1)+group("g")), "ClusterRoleBinding cb binds ClusterRole cr to [{Kind:User APIGroup: Name:u Namespace:} {Kind:ServiceAccount"},
		{"another ServiceAccount's ClusterRoleBinding to another ClusterRole before the manager's", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole2, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr2", account("other", "ns")), clusterRole, clusterBinding), "ClusterRoleBinding cb2 binds ClusterRole cr2 to {Kind:ServiceAccount APIGroup: Name:other Namespace:ns}: a binding may name only"},
		{"another ServiceAccount's ClusterRoleBinding to the manager's ClusterRole", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr", account("other", "ns")), clusterBinding), "ClusterRoleBinding cb2 binds ClusterRole cr to [{Kind:ServiceAccount APIGroup: Name:other Namespace:ns}]"},
		{"another ServiceAccount's RoleBinding to the Role", with(bindingOf("RoleBinding", "b2", "Role", "r", account("other", "ns"))), "RoleBinding b2 binds Role ns/r to [{Kind:ServiceAccount APIGroup: Name:other Namespace:ns}]"},
		{"the Role bound twice to the manager alone", with(bindingOf("RoleBinding", "b2", "Role", "r", manager)), ""},
		{"the ClusterRole bound twice to the manager alone", with(bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr", manager)), ""},
		{"a second manager without a ClusterRoleBinding", manifest(with(), strings.Replace(strings.Replace(deployment, "name: d\n", "name: e\n", 1), "serviceAccountName: manager", "serviceAccountName: other", 1), strings.Replace(binding, "name: manager\n", "name: other\n", 1)), `the ServiceAccount "other" of Deployment e reaches the ClusterRoles []`},
		{"a second manager without a grant", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, strings.Replace(strings.Replace(deployment, "name: d\n", "name: e\n", 1), "serviceAccountName: manager", "serviceAccountName: other", 1), grant, binding, clusterRole, clusterBinding), `the ServiceAccount "other" of Deployment e reaches the Roles []`},

		{"a second Role bound to the manager", with(role2, bindingOf("RoleBinding", "b2", "Role", "r2", manager)), "reaches the Roles [Role ns/r Role ns/r2]"},
		{"a second ClusterRole bound to the manager", with(clusterRole2, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr2", manager)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cr2]"},
		{"a ClusterRole bound to the manager by a RoleBinding", with(clusterRole2, bindingOf("RoleBinding", "b2", "ClusterRole", "cr2", manager)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cr2]"},
		{"cluster-admin bound to the manager", with(bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cluster-admin", manager)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cluster-admin]"},
		{"cluster-admin bound to the manager in the namespace", with(bindingOf("RoleBinding", "b2", "ClusterRole", "cluster-admin", manager)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cluster-admin]"},

		{"the Role without its Events rule", withRole(createLeases + namedLease(lease, "get, update")), "are not the leader election rules"},
		{"the Role without create on leases", withRole(events + namedLease(lease, "get, update")), "are not the leader election rules"},
		{"the Role without get and update on its Lease", withRole(events + createLeases), "are not the leader election rules"},
		{"the Role without rules", withRole(""), "are not the leader election rules"},
		{"the Role with the grant of one rule on every Lease", withRole(events + createLeases + "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [get, update]\n"), "are not the leader election rules"},
		{"the Role with the grant of one rule on every Lease and no names", withRole("- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [create, get, update]\n"), "are not the leader election rules"},
		{"the Role with the grant on a Lease of another name", withRole(events + createLeases + namedLease("x", "get, update")), "are not the leader election rules"},
		{"the Role with the grant on two Leases", withRole(events + createLeases + namedLease(lease+", x", "get, update")), "are not the leader election rules"},
		{"the Role with delete on its Lease", withRole(events + createLeases + namedLease(lease, "get, update, delete")), "are not the leader election rules"},
		{"the Role with every verb on its Lease", withRole(events + createLeases + namedLease(lease, "'*'")), "are not the leader election rules"},
		{"the Role with a rule on every resource", withRole(leaderElection + everything), "are not the leader election rules"},
		{"the Role with a rule on every resource instead", withRole(everything), "are not the leader election rules"},
		{"the Role with a rule on Secrets", withRole(leaderElection + secrets), "are not the leader election rules"},
		{"the Role with delete on every Lease", withRole(leaderElection + "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [delete]\n"), "are not the leader election rules"},
		{"the Role with another resource of the API group", withRole(events + strings.Replace(createLeases, "[leases]", "[leasecandidates]", 1) + namedLease(lease, "get, update")), "are not the leader election rules"},
		{"the Role with leases of another API group", withRole(events + strings.Replace(createLeases, "coordination.k8s.io", "example.io", 1) + namedLease(lease, "get, update")), "are not the leader election rules"},
		{"the Role with delete on Events", withRole(strings.Replace(events, "patch", "patch, delete", 1) + createLeases + namedLease(lease, "get, update")), "are not the leader election rules"},
		{"the Role with an empty resourceNames beside the rules", withRole(strings.Replace(events, "  verbs", "  resourceNames: []\n  verbs", 1) + createLeases + namedLease(lease, "get, update")), ""},
		{"resourceNames with another case", withRole(events + createLeases + strings.Replace(namedLease(lease, "get, update"), "resourceNames", "resourcenames", 1)), `unknown field "rules[2].resourcenames"`},
		{"an unknown field of a rule", withRole(leaderElection + "  verb: [delete]\n"), `unknown field "rules[2].verb"`},
		{"an unknown field of a Role", withRole(leaderElection + "extra: x\n"), `unknown field "extra"`},
		{"a key of another case in a RoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, strings.Replace(binding, "roleRef:", "roleref:", 1), clusterRole, clusterBinding), "reaches the Roles [], want the leader election Role in ns alone"},
		{"a key of another case in a ClusterRole", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, strings.Replace(clusterRole, "apiGroups", "apigroups", 1), clusterBinding), `unknown field "rules[0].apigroups"`},
		{"a key of another case in a ClusterRoleBinding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "subjects:", "Subjects:", 1)), "reaches the ClusterRoles [], want the manager's ClusterRole alone"},

		{"the ClusterRole with another resource of lodia.io", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRoleHead+lodiaKinds+secrets+"- apiGroups: [lodia.io]\n  resources: [controlplanes]\n  verbs: [get]\n", clusterBinding), ""},
		{"the ClusterRole with another resource of the API group of leases", withClusterRole(secrets + "- apiGroups: [coordination.k8s.io]\n  resources: [leasecandidates]\n  verbs: [get]\n"), ""},
		{"the ClusterRole with leases of another API group", withClusterRole(secrets + "- apiGroups: [example.io]\n  resources: [leases]\n  verbs: [get]\n"), ""},
		{"the ClusterRole with events of another API group", withClusterRole(secrets + "- apiGroups: [example.io]\n  resources: [events]\n  verbs: [get]\n"), ""},
		{"the ClusterRole with an aggregationRule", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, strings.Replace(clusterRole, "rules:\n", "aggregationRule:\n  clusterRoleSelectors:\n  - matchLabels:\n      a: b\nrules:\n", 1), clusterBinding), "ClusterRole cr has an aggregationRule"},
		{"the ClusterRole with an empty aggregationRule", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, strings.Replace(clusterRole, "rules:\n", "aggregationRule: {}\nrules:\n", 1), clusterBinding), "ClusterRole cr has an aggregationRule"},
		{"the ClusterRole with get on a Lease of a name", withClusterRole(secrets + fmt.Sprintf(named, "x", "get")), `rule 1 of ClusterRole cr grants [get] on leases in the group "coordination.k8s.io"`},
		{"the ClusterRole with the grant in its last rule", withClusterRole(secrets + lodiaKinds + "- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [update]\n"), `rule 2 of ClusterRole cr grants [update] on leases`},
		{"the ClusterRole with the grant in its first rule", withClusterRole("- apiGroups: [coordination.k8s.io]\n  resources: [leases]\n  verbs: [update]\n" + secrets), `rule 0 of ClusterRole cr grants [update] on leases`},
		{"the ClusterRole with the grant among several groups and resources", withClusterRole("- apiGroups: [xx, coordination.k8s.io, zz]\n  resources: [aa, leases, bb]\n  verbs: [get]\n"), `grants [get] on leases in the group "coordination.k8s.io"`},
		{"the ClusterRole with a rule on every group and every resource", withClusterRole(everything), `grants [*] on leases in the group "coordination.k8s.io"`},
		{"the ClusterRole with a rule on every resource of the group of leases", withClusterRole("- apiGroups: [coordination.k8s.io]\n  resources: ['*']\n  verbs: [get]\n"), `on leases in the group "coordination.k8s.io"`},
		{"the ClusterRole with a rule on leases of every group", withClusterRole("- apiGroups: ['*']\n  resources: [leases]\n  verbs: [get]\n"), `on leases in the group "coordination.k8s.io"`},
		{"the ClusterRole with get on Events", withClusterRole(secrets + "- apiGroups: ['']\n  resources: [events]\n  verbs: [get]\n"), `rule 1 of ClusterRole cr grants [get] on events in the group ""`},
		{"the ClusterRole with a rule on every resource of the core group", withClusterRole("- apiGroups: ['']\n  resources: ['*']\n  verbs: [get]\n"), `on events in the group ""`},
		{"the ClusterRole with a rule on Events of every group", withClusterRole("- apiGroups: ['*']\n  resources: [events]\n  verbs: [get]\n"), `on events in the group ""`},
		{"the ClusterRole with get on Events of events.k8s.io", withClusterRole(secrets + "- apiGroups: [events.k8s.io]\n  resources: [events]\n  verbs: [get]\n"), `on events in the group "events.k8s.io"`},
		{"the ClusterRole with a rule on every resource of events.k8s.io", withClusterRole("- apiGroups: [events.k8s.io]\n  resources: ['*']\n  verbs: [get]\n"), `on events in the group "events.k8s.io"`},
	}

	strangers := []struct{ name, subject string }{
		{"a user", user("u")},
		{"a group", group("g")},
		{"a ServiceAccount of another name", account("other", "ns")},
		{"the user form of another ServiceAccount", user("system:serviceaccount:ns:other")},
		{"the user form of a name that ends like the manager's", user("system:serviceaccount:ns:other-manager")},
		{"the user form of a name that starts like the manager's", user("system:serviceaccount:ns:manager2")},
		{"the user form without a namespace", user("system:serviceaccount:manager")},
		{"the user form with an empty namespace", user("system:serviceaccount::manager")},
		{"the user form with a longer namespace", user("system:serviceaccount:ns:x:manager")},
		{"a user named like the ServiceAccount", user("manager")},
		{"a group named like the ServiceAccount", group("manager")},
		{"the group of every ServiceAccount, longer", group("system:serviceaccountsx")},
		{"the group of a namespace's ServiceAccounts, without its namespace", group("system:serviceaccounts:")},
		{"the unauthenticated group", group("system:unauthenticated")},
		{"the masters group", group("system:masters")},
		{"a user named like the group of every authenticated user", user("system:authenticated")},
		{"a group named like the user form", group("system:serviceaccount:ns:manager")},
		{"a user named like the group of every ServiceAccount", user("system:serviceaccounts")},
	}
	for _, c := range strangers {
		cases = append(cases,
			renderCase{c.name + " bound to another ClusterRole", with(clusterRole2, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr2", c.subject)), "a binding may name only the manager's ServiceAccount, by name and namespace"},
			renderCase{c.name + " bound to another Role", with(role2, bindingOf("RoleBinding", "b2", "Role", "r2", c.subject)), "a binding may name only the manager's ServiceAccount, by name and namespace"},
		)
	}

	reaching := []struct{ name, subject string }{
		{"the user form", user("system:serviceaccount:ns:manager")},
		{"the user form in another namespace", user("system:serviceaccount:other:manager")},
		{"every ServiceAccount", group("system:serviceaccounts")},
		{"the ServiceAccounts of the namespace", group("system:serviceaccounts:ns")},
		{"the ServiceAccounts of another namespace", group("system:serviceaccounts:other")},
		{"every authenticated user", group("system:authenticated")},
	}
	for _, c := range reaching {
		cases = append(cases,
			renderCase{c.name + " added to the Role's binding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding+c.subject, clusterRole, clusterBinding), "RoleBinding b binds Role ns/r to"},
			renderCase{c.name + " added to the ClusterRole's binding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, clusterBinding+c.subject), "ClusterRoleBinding cb binds ClusterRole cr to"},
			renderCase{c.name + " in a second binding to the Role", with(bindingOf("RoleBinding", "b2", "Role", "r", c.subject)), "RoleBinding b2 binds Role ns/r to"},
			renderCase{c.name + " in a second binding to the ClusterRole", with(bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr", c.subject)), "ClusterRoleBinding cb2 binds ClusterRole cr to"},
			renderCase{c.name + " in a RoleBinding to the ClusterRole", with(bindingOf("RoleBinding", "b2", "ClusterRole", "cr", c.subject)), "RoleBinding b2 binds ClusterRole cr to"},
			renderCase{c.name + " bound to another ClusterRole", with(clusterRole2, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr2", c.subject)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cr2]"},
			renderCase{c.name + " bound to another Role", with(role2, bindingOf("RoleBinding", "b2", "Role", "r2", c.subject)), "reaches the Roles [Role ns/r Role ns/r2]"},
		)
	}

	elsewhere := account("manager", "other")
	nowhere := "- kind: ServiceAccount\n  name: manager\n"
	cases = append(cases, []renderCase{
		{"the ServiceAccount in another namespace added to the Role's binding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding+elsewhere, clusterRole, clusterBinding), "RoleBinding b binds Role ns/r to"},
		{"the ServiceAccount in another namespace added to the ClusterRole's binding", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, clusterBinding+elsewhere), "ClusterRoleBinding cb binds ClusterRole cr to"},
		{"the ServiceAccount in another namespace bound to another ClusterRole", with(clusterRole2, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr2", elsewhere)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cr2]"},
		{"the ServiceAccount in another namespace bound to another Role", with(role2, bindingOf("RoleBinding", "b2", "Role", "r2", elsewhere)), "reaches the Roles [Role ns/r Role ns/r2]"},
		{"the ServiceAccount in another namespace in a second binding to the Role is the manager's once clusterctl rewrites it", with(bindingOf("RoleBinding", "b2", "Role", "r", elsewhere)), ""},
		{"the ServiceAccount in another namespace in a second binding to the ClusterRole is the manager's once clusterctl rewrites it", with(bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr", elsewhere)), ""},
		{"the ServiceAccount in another namespace in a RoleBinding to the ClusterRole is the manager's once clusterctl rewrites it", with(bindingOf("RoleBinding", "b2", "ClusterRole", "cr", elsewhere)), ""},
		{"the ServiceAccount without a namespace bound to another ClusterRole in the namespace", with(clusterRole2, bindingOf("RoleBinding", "b2", "ClusterRole", "cr2", nowhere)), "reaches the ClusterRoles [ClusterRole cr ClusterRole cr2]"},
		{"the ServiceAccount without a namespace bound to another Role", with(role2, bindingOf("RoleBinding", "b2", "Role", "r2", nowhere)), "reaches the Roles [Role ns/r Role ns/r2]"},
		{"the ClusterRoleBinding's subject without a namespace", manifest(namespace, bootstrapRole, bootstrapBinding, deployment, grant, binding, clusterRole, strings.Replace(clusterBinding, "  name: manager\n  namespace: ns\n", "  name: manager\n", 1)), "ClusterRoleBinding cb binds ClusterRole cr to [{Kind:ServiceAccount APIGroup: Name:manager Namespace:}]"},
	}...)
	runCases(t, cases)
}
func TestTheKubeadmBootstrapProvidersGrantIsGetOnLodiaControlPlanesAlone(t *testing.T) {
	without := func(docs ...string) string {
		return manifest(append([]string{namespace, deployment, grant, binding, clusterRole, clusterBinding}, docs...)...)
	}
	asRole := func(rules string) string { return bootstrapRoleHead + rules }
	sa := account("capi-kubeadm-bootstrap-manager", "capi-kubeadm-bootstrap-system")
	subjectOf := func(subjects string) string { return strings.Replace(bootstrapBinding, bootstrapSubject, subjects, 1) }
	bindOf := func(kind, role string) string {
		return strings.Replace(strings.Replace(bootstrapBinding, "kind: ClusterRoleBinding", "kind: "+kind, 1), "  kind: ClusterRole\n  name: bootstrap\n", "  kind: "+role+"\n  name: bootstrap\n", 1)
	}
	cases := []renderCase{
		{"the grant as shipped", without(bootstrapRole, bootstrapBinding), ""},
		{"no grant at all", without(), "no binding names the User system:serviceaccount:capi-kubeadm-bootstrap-system:capi-kubeadm-bootstrap-manager"},
		{"a role and no binding", without(bootstrapRole), "no binding names the User"},
		{"a binding left on a ServiceAccount subject", without(bootstrapRole, subjectOf(sa)), "no binding names the User"},
		{"a binding left on a ServiceAccount subject, which clusterctl rewrites", without(bootstrapRole, subjectOf(sa)), "ClusterRoleBinding bootstrap binds ClusterRole bootstrap to {Kind:ServiceAccount APIGroup: Name:capi-kubeadm-bootstrap-manager Namespace:ns}: a binding may name only"},
		{"a ClusterRole widened to update", without(strings.Replace(bootstrapRole, "verbs: [get]", "verbs: [get, update]", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole that grants list instead", without(strings.Replace(bootstrapRole, "verbs: [get]", "verbs: [list]", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole that grants every verb", without(strings.Replace(bootstrapRole, "verbs: [get]", "verbs: ['*']", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole with no verb", without(strings.Replace(bootstrapRole, "verbs: [get]", "verbs: []", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole on the status too", without(strings.Replace(bootstrapRole, "resources: [lodiacontrolplanes]", "resources: [lodiacontrolplanes, lodiacontrolplanes/status]", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole on every resource of the group", without(strings.Replace(bootstrapRole, "resources: [lodiacontrolplanes]", "resources: ['*']", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole on another resource of the group", without(strings.Replace(bootstrapRole, "lodiacontrolplanes", "lodiacontrolplanetemplates", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole on every group", without(strings.Replace(bootstrapRole, "apiGroups: [controlplane.lodia.io]", "apiGroups: ['*']", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole on another group", without(strings.Replace(bootstrapRole, "controlplane.lodia.io", "lodia.io", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole limited to a name", without(strings.Replace(bootstrapRole, "  verbs: [get]", "  resourceNames: [x]\n  verbs: [get]", 1), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole with a second rule", without(bootstrapRole+secrets, bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole with no rule", without(asRole(""), bootstrapBinding), "are not get on lodiacontrolplanes alone"},
		{"a ClusterRole with an aggregationRule", without(strings.Replace(bootstrapRole, "rules:\n", "aggregationRule:\n  clusterRoleSelectors:\n  - matchLabels:\n      a: b\nrules:\n", 1), bootstrapBinding), "ClusterRole bootstrap has an aggregationRule"},
		{"a RoleBinding instead", without(bootstrapRole, bindOf("RoleBinding", "ClusterRole")), "grants only in one namespace"},
		{"a binding to a ClusterRole that is not rendered", without(bootstrapRole, strings.Replace(bootstrapBinding, "  name: bootstrap\nsubjects", "  name: missing\nsubjects", 1)), "want a ClusterRole that config/default renders"},
		{"a binding to a Role", without(bootstrapRole, bindOf("ClusterRoleBinding", "Role")), "want a ClusterRole that config/default renders"},
		{"the User bound to a second ClusterRole", without(bootstrapRole, bootstrapBinding, clusterRole2(), bindingOf("ClusterRoleBinding", "bootstrap2", "ClusterRole", "cr2", bootstrapSubject)), "reaches the roles [ClusterRole bootstrap ClusterRole cr2]"},
		{"another User in the binding", without(bootstrapRole, bootstrapBinding+user("u")), "a binding may name only"},
		{"a group in the binding", without(bootstrapRole, bootstrapBinding+group("system:authenticated")), "a binding may name only"},
		{"a ServiceAccount of the namespace in the binding", without(bootstrapRole, bootstrapBinding+account("other", "ns")), "a binding may name only"},
		{"the ServiceAccount of the manager in the binding", without(bootstrapRole, bootstrapBinding+account("manager", "ns")), "reaches the ClusterRoles [ClusterRole cr ClusterRole bootstrap]"},
		{"the manager's ClusterRole bound to the User", without(bootstrapRole, bootstrapBinding, bindingOf("ClusterRoleBinding", "cb2", "ClusterRole", "cr", bootstrapSubject)), "reaches the roles [ClusterRole bootstrap ClusterRole cr]"},
		{"no variable for the namespace", strings.Replace(without(bootstrapRole, bootstrapBinding), "${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE:=capi-kubeadm-bootstrap-system}", "capi-kubeadm-bootstrap-system", 1), "does not reference the variable LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE"},
		{"no variable for the name", strings.Replace(without(bootstrapRole, bootstrapBinding), "${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=capi-kubeadm-bootstrap-manager}", "capi-kubeadm-bootstrap-manager", 1), "does not reference the variable LODIA_KUBEADM_BOOTSTRAP_SA_NAME"},
	}
	runCases(t, cases)
}

func clusterRole2() string { return rename(clusterRole, "cr", "cr2") }

func TestClusterScopedIsWhatClusterctlLeavesOutOfTheTargetNamespace(t *testing.T) {
	gvks := map[string]schema.GroupVersionKind{}
	for gvk := range clientgoscheme.Scheme.AllKnownTypes() {
		if known, ok := gvks[gvk.Kind]; !strings.HasSuffix(gvk.Kind, "List") && (!ok || gvk.Version < known.Version) {
			gvks[gvk.Kind] = gvk
		}
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy"},
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"},
		{Group: "cert-manager.io", Version: "v1", Kind: "Issuer"},
		{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"},
		{Group: "apiregistration.k8s.io", Version: "v1", Kind: "APIService"},
	} {
		gvks[gvk.Kind] = gvk
	}
	for _, kind := range append(slices.Clone(clusterScoped), outsideEveryNamespace...) {
		if _, ok := gvks[kind]; !ok {
			gvks[kind] = schema.GroupVersionKind{Version: "v1", Kind: kind}
		}
	}
	var objs []unstructured.Unstructured
	for _, gvk := range gvks {
		obj := unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetName("obj")
		objs = append(objs, obj)
	}
	raw, err := utilyaml.FromUnstructured(objs)
	if err != nil {
		t.Fatal(err)
	}
	configClient, err := config.New(context.Background(), "", config.InjectReader(config.NewMemoryReader()))
	if err != nil {
		t.Fatal(err)
	}
	components, err := repository.NewComponents(repository.ComponentsInput{
		Provider:     config.NewProvider("lodia", "https://example.invalid", clusterctlv1.ControlPlaneProviderType),
		ConfigClient: configClient,
		Processor:    yamlprocessor.NewSimpleProcessor(),
		RawYaml:      raw,
		Options:      repository.ComponentsOptions{TargetNamespace: "target", SkipTemplateProcess: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(components.Objs()) != len(gvks) {
		t.Fatalf("clusterctl returned %d objects for %d kinds", len(components.Objs()), len(gvks))
	}
	for _, obj := range components.Objs() {
		left := obj.GetNamespace() != "target"
		if held := slices.Contains(clusterScoped, obj.GetKind()); left != held {
			t.Errorf("%s: clusterctl leaves it out of the target namespace: %v, clusterScoped holds it: %v", obj.GetKind(), left, held)
		}
	}
}
