// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Command components reads the rendered config/default on its standard input
// and fails unless:
//   - it holds one Namespace, and every object of a namespaced kind is in it;
//   - every object has the label cluster.x-k8s.io/provider;
//   - a Deployment has a container named manager;
//   - a RoleBinding in the manager's namespace gives the manager's
//     ServiceAccount a Role that grants create, get and update on leases;
//   - a ClusterRoleBinding gives the manager's ServiceAccount a ClusterRole
//     that the render holds.
//
// The first three are rules of what clusterctl asks of a components YAML
// (https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md#components-yaml);
// the last two are what the manager needs to run.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const providerLabel = "cluster.x-k8s.io/provider"

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
	for _, manager := range managers {
		errs = append(errs, requireLeaderElection(objs, manager), requireClusterRole(objs, manager))
	}
	return errors.Join(errs...)
}

func requireLeaderElection(objs []*unstructured.Unstructured, manager *unstructured.Unstructured) error {
	ns := manager.GetNamespace()
	account := serviceAccount(manager)
	roles := make(map[string]rbacv1.Role)
	var bindings []rbacv1.RoleBinding
	for _, obj := range objs {
		if obj.GetNamespace() != ns {
			continue
		}
		switch obj.GetKind() {
		case "Role":
			var role rbacv1.Role
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role); err != nil {
				return fmt.Errorf("decode Role %s: %w", obj.GetName(), err)
			}
			roles[role.Name] = role
		case "RoleBinding":
			var binding rbacv1.RoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &binding); err != nil {
				return fmt.Errorf("decode RoleBinding %s: %w", obj.GetName(), err)
			}
			bindings = append(bindings, binding)
		}
	}
	for _, binding := range bindings {
		if binding.RoleRef.Kind != "Role" || !grantsLeases(roles[binding.RoleRef.Name].Rules) {
			continue
		}
		if boundTo(binding.Subjects, account, ns) {
			return nil
		}
	}
	return fmt.Errorf("the ServiceAccount %q of Deployment %s has no RoleBinding in %s to a Role that grants create, get and update on leases: the manager could not take its leader election Lease", account, manager.GetName(), ns)
}

func requireClusterRole(objs []*unstructured.Unstructured, manager *unstructured.Unstructured) error {
	ns := manager.GetNamespace()
	account := serviceAccount(manager)
	var roles []string
	var bindings []rbacv1.ClusterRoleBinding
	for _, obj := range objs {
		switch obj.GetKind() {
		case "ClusterRole":
			roles = append(roles, obj.GetName())
		case "ClusterRoleBinding":
			var binding rbacv1.ClusterRoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &binding); err != nil {
				return fmt.Errorf("decode ClusterRoleBinding %s: %w", obj.GetName(), err)
			}
			bindings = append(bindings, binding)
		}
	}
	for _, binding := range bindings {
		if binding.RoleRef.Kind == "ClusterRole" && slices.Contains(roles, binding.RoleRef.Name) && boundTo(binding.Subjects, account, ns) {
			return nil
		}
	}
	return fmt.Errorf("the ServiceAccount %q of Deployment %s has no ClusterRoleBinding to a ClusterRole that config/default renders: the manager could not read a Cluster or a LodiaControlPlane", account, manager.GetName())
}

func serviceAccount(manager *unstructured.Unstructured) string {
	account, _, _ := unstructured.NestedString(manager.Object, "spec", "template", "spec", "serviceAccountName")
	return account
}

func boundTo(subjects []rbacv1.Subject, account, ns string) bool {
	return slices.ContainsFunc(subjects, func(s rbacv1.Subject) bool {
		return s.Kind == rbacv1.ServiceAccountKind && s.Name == account && s.Namespace == ns
	})
}

func grantsLeases(rules []rbacv1.PolicyRule) bool {
	granted := make(map[string]bool)
	for _, rule := range rules {
		if covers(rule.APIGroups, "coordination.k8s.io") && covers(rule.Resources, "leases") {
			for _, verb := range rule.Verbs {
				granted[verb] = true
			}
		}
	}
	for _, verb := range []string{"create", "get", "update"} {
		if !granted[verb] && !granted[rbacv1.VerbAll] {
			return false
		}
	}
	return true
}

func covers(list []string, name string) bool {
	return slices.Contains(list, name) || slices.Contains(list, rbacv1.ResourceAll)
}
