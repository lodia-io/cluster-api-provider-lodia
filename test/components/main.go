// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Command components reads the rendered config/default on its standard input
// and fails unless:
//   - it holds one Namespace, and every object of a namespaced kind is in it;
//   - every object has the label cluster.x-k8s.io/provider;
//   - a Deployment has a container named manager;
//   - the roles that reach the manager's ServiceAccount are one Role, in the
//     manager's namespace, and one ClusterRole, and every binding to either
//     names the ServiceAccount alone;
//   - the Role holds the rules of leader election and no other: create on
//     events, and on leases, and get and update on the Lease that the manager's
//     LeaderElectionID names;
//   - a ClusterRoleBinding gives the manager's ServiceAccount a ClusterRole
//     that the render holds and that has no aggregationRule and no rule on
//     leases or events.
//
// The first three are rules of what clusterctl asks of a components YAML
// (https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md#components-yaml);
// the rest are what the manager needs to run and no more, held on the render
// because clusterctl installs every object of it.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

const (
	providerLabel = "cluster.x-k8s.io/provider"

	serviceAccountUserPrefix  = "system:serviceaccount:"
	serviceAccountsGroup      = "system:serviceaccounts"
	serviceAccountGroupPrefix = serviceAccountsGroup + ":"
	authenticatedGroup        = "system:authenticated"
)

// clusterScoped is clusterctl's list of the kinds it leaves outside the target
// namespace. It is internal to Cluster API, so this is a copy:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/internal/util/objs.go#L81-L103
var clusterScoped = []string{
	"Namespace",
	"Node",
	"PersistentVolume",
	"PodSecurityPolicy",
	"CertificateSigningRequest",
	"ClusterRoleBinding",
	"ClusterRole",
	"VolumeAttachment",
	"StorageClass",
	"CSIDriver",
	"CSINode",
	"ValidatingWebhookConfiguration",
	"MutatingWebhookConfiguration",
	"CustomResourceDefinition",
	"PriorityClass",
	"RuntimeClass",
}

func main() {
	if err := check(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(r io.Reader) error {
	options, err := controller.ManagerOptions("0", "0", true)
	if err != nil {
		return err
	}
	var objs []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if len(obj.Object) == 0 {
			continue
		}
		objs = append(objs, obj)
	}
	var errs []error
	var namespaces []string
	var managers []*unstructured.Unstructured
	for _, obj := range objs {
		if _, ok := obj.GetLabels()[providerLabel]; !ok {
			errs = append(errs, fmt.Errorf("%s %s has no label %s", obj.GetKind(), obj.GetName(), providerLabel))
		}
		switch obj.GetKind() {
		case "Namespace":
			namespaces = append(namespaces, obj.GetName())
		case "Deployment":
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			if slices.ContainsFunc(containers, func(c any) bool {
				container, ok := c.(map[string]any)
				return ok && container["name"] == "manager"
			}) {
				managers = append(managers, obj)
			}
		}
	}
	if len(namespaces) != 1 {
		errs = append(errs, fmt.Errorf("config/default renders %d Namespaces, clusterctl wants one", len(namespaces)))
	} else {
		for _, obj := range objs {
			if !slices.Contains(clusterScoped, obj.GetKind()) && obj.GetNamespace() != namespaces[0] {
				errs = append(errs, fmt.Errorf("%s %s is in the namespace %q, not in the Namespace %s: the contract wants every namespaced object in it", obj.GetKind(), obj.GetName(), obj.GetNamespace(), namespaces[0]))
			}
		}
	}
	if len(managers) == 0 {
		errs = append(errs, errors.New("config/default renders no Deployment with a container named manager, which clusterctl looks for"))
	}
	access, err := readRBAC(objs)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, manager := range managers {
		errs = append(errs, requireManagerRBAC(access, manager, options.LeaderElectionID))
	}
	return errors.Join(errs...)
}

type roleID struct{ kind, namespace, name string }

func (r roleID) String() string {
	if r.namespace == "" {
		return r.kind + " " + r.name
	}
	return r.kind + " " + r.namespace + "/" + r.name
}

type rolePolicy struct {
	rules       []rbacv1.PolicyRule
	aggregation *rbacv1.AggregationRule
}

type roleGrant struct {
	kind, name string
	role       roleID
	subjects   []rbacv1.Subject
}

type access struct {
	roles  map[roleID]rolePolicy
	grants []roleGrant
}

// The API server reads a key by exact case, and clusterctl creates objects
// without field validation, which the API server defaults to Warn: a key like
// resourcenames is dropped with a warning, and the rule grants more. So an
// unknown field is an error here.
func decode(obj *unstructured.Unstructured, into any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(obj.Object, into, true); err != nil {
		return fmt.Errorf("decode %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

func readRBAC(objs []*unstructured.Unstructured) (access, error) {
	a := access{roles: make(map[roleID]rolePolicy)}
	var errs []error
	for _, obj := range objs {
		var err error
		switch obj.GetKind() {
		case "Role":
			var r rbacv1.Role
			if err = decode(obj, &r); err == nil {
				a.roles[roleID{"Role", obj.GetNamespace(), obj.GetName()}] = rolePolicy{rules: r.Rules}
			}
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err = decode(obj, &r); err == nil {
				a.roles[roleID{"ClusterRole", "", obj.GetName()}] = rolePolicy{rules: r.Rules, aggregation: r.AggregationRule}
			}
		case "RoleBinding":
			var b rbacv1.RoleBinding
			if err = decode(obj, &b); err == nil {
				ref := roleID{b.RoleRef.Kind, "", b.RoleRef.Name}
				if ref.kind == "Role" {
					ref.namespace = obj.GetNamespace()
				}
				a.grants = append(a.grants, roleGrant{obj.GetKind(), obj.GetName(), ref, b.Subjects})
			}
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			if err = decode(obj, &b); err == nil {
				a.grants = append(a.grants, roleGrant{obj.GetKind(), obj.GetName(), roleID{b.RoleRef.Kind, "", b.RoleRef.Name}, b.Subjects})
			}
		}
		errs = append(errs, err)
	}
	return a, errors.Join(errs...)
}

func requireManagerRBAC(a access, manager *unstructured.Unstructured, lease string) error {
	ns := manager.GetNamespace()
	account := serviceAccount(manager)
	who := fmt.Sprintf("the ServiceAccount %q of Deployment %s", account, manager.GetName())
	var reaching, roles, clusterRoles []roleID
	for _, g := range a.grants {
		if g.reaches(account) && !slices.Contains(reaching, g.role) {
			reaching = append(reaching, g.role)
			switch g.role.kind {
			case "Role":
				roles = append(roles, g.role)
			case "ClusterRole":
				clusterRoles = append(clusterRoles, g.role)
			}
		}
	}
	var errs []error
	switch {
	case len(roles) != 1 || roles[0].namespace != ns:
		errs = append(errs, fmt.Errorf("%s reaches the Roles %v, want the leader election Role in %s alone", who, roles, ns))
	case !a.has(roles[0]):
		errs = append(errs, fmt.Errorf("%s has a RoleBinding to %s, which config/default does not render", who, roles[0]))
	default:
		if diff := cmp.Diff(leaderElectionRules(lease), a.roles[roles[0]].rules, cmpopts.EquateEmpty()); diff != "" {
			errs = append(errs, fmt.Errorf("the rules of %s are not the leader election rules (-want +got):\n%s", roles[0], diff))
		}
	}
	switch {
	case len(clusterRoles) != 1:
		errs = append(errs, fmt.Errorf("%s reaches the ClusterRoles %v, want the manager's ClusterRole alone", who, clusterRoles))
	case !a.has(clusterRoles[0]):
		errs = append(errs, fmt.Errorf("%s has a binding to %s, which config/default does not render", who, clusterRoles[0]))
	case !a.clusterBound(clusterRoles[0], account):
		errs = append(errs, fmt.Errorf("%s has no ClusterRoleBinding to %s: the manager could not read a Cluster or a LodiaControlPlane", who, clusterRoles[0]))
	default:
		errs = append(errs, requireNoLeasesOrEvents(clusterRoles[0], a.roles[clusterRoles[0]]))
	}
	alone := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: account, Namespace: ns}}
	for _, g := range a.grants {
		if slices.Contains(reaching, g.role) && !slices.Equal(g.subjects, alone) {
			errs = append(errs, fmt.Errorf("%s %s binds %s to %+v, want %s alone", g.kind, g.name, g.role, g.subjects, who))
		}
	}
	return errors.Join(errs...)
}

func (a access) has(id roleID) bool {
	_, ok := a.roles[id]
	return ok
}

func (a access) clusterBound(id roleID, account string) bool {
	return slices.ContainsFunc(a.grants, func(g roleGrant) bool {
		return g.kind == "ClusterRoleBinding" && g.role == id && g.reaches(account)
	})
}

// A create request has no name yet to match, so create on leases is a rule of
// its own.
func leaderElectionRules(lease string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{lease}, Verbs: []string{"get", "update"}},
	}
}

func requireNoLeasesOrEvents(id roleID, r rolePolicy) error {
	var errs []error
	if r.aggregation != nil {
		errs = append(errs, fmt.Errorf("%s has an aggregationRule, which adds rules that config/default does not render", id))
	}
	for i, rule := range r.rules {
		for _, c := range []struct{ group, resource string }{
			{"coordination.k8s.io", "leases"},
			{"", "events"},
			{"events.k8s.io", "events"},
		} {
			if covers(rule.APIGroups, c.group) && covers(rule.Resources, c.resource) {
				errs = append(errs, fmt.Errorf("rule %d of %s grants %v on %s in the group %q, where only the leader election Role grants", i, id, rule.Verbs, c.resource, c.group))
			}
		}
	}
	return errors.Join(errs...)
}

func serviceAccount(manager *unstructured.Unstructured) string {
	account, _, _ := unstructured.NestedString(manager.Object, "spec", "template", "spec", "serviceAccountName")
	return account
}

func (g roleGrant) reaches(account string) bool {
	return slices.ContainsFunc(g.subjects, func(s rbacv1.Subject) bool { return subjectReaches(s, account) })
}

// subjectReaches reports whether the RBAC authorizer applies the subject to the
// manager's ServiceAccount: to the user system:serviceaccount:<namespace>:<name>
// in the groups system:serviceaccounts, system:serviceaccounts:<namespace> and
// system:authenticated.
// clusterctl sets the namespace of every ServiceAccount subject to the one it
// installs the provider in, which the installer picks, so a ServiceAccount
// subject of the manager's name reaches it in any namespace, and so do the user
// and the namespace group of any namespace.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L384
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/registry/rbac/validation/rule.go#L281-L304
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go#L29-L33
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go#L126-L136
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/authentication/group/authenticated_group_adder.go#L56
func subjectReaches(s rbacv1.Subject, account string) bool {
	switch s.Kind {
	case rbacv1.ServiceAccountKind:
		return s.Name == account
	case rbacv1.UserKind:
		rest, ok := strings.CutPrefix(s.Name, serviceAccountUserPrefix)
		ns, name, found := strings.Cut(rest, ":")
		return ok && found && ns != "" && name == account
	case rbacv1.GroupKind:
		ns, namespaced := strings.CutPrefix(s.Name, serviceAccountGroupPrefix)
		return s.Name == serviceAccountsGroup || s.Name == authenticatedGroup || namespaced && ns != ""
	}
	return false
}

func covers(list []string, name string) bool {
	return slices.Contains(list, name) || slices.Contains(list, rbacv1.ResourceAll)
}
