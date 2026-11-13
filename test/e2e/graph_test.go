// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A graph that misses one of these has lost its CRD's clusterctl label, and
// every check on it would pass blind.
var graphKinds = []string{
	"Cluster", "LodiaControlPlane", "DevCluster", "DevMachineTemplate", "KubeadmConfigTemplate",
	"MachineDeployment", "MachineSet", "Machine", "DevMachine", "KubeadmConfig",
	"ControlPlane", "Etcd", "Secret",
}

// listGraph lists what clusterctl's GetOwnerGraph lists, on the suite's client,
// since GetOwnerGraph builds a proxy of its own from a kubeconfig path. Lodia's
// ControlPlane and etcd-druid's Etcd are added, since their CRDs carry no
// clusterctl label. The cluster-scoped kinds are left out: the quick start
// keeps them only when their names contain the Cluster's, and the suite
// creates none.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/ownergraph.go#L71-L149
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/objectgraph.go#L346-L412
func listGraph(ctx context.Context, c client.Reader, namespace string) ([]*unstructured.Unstructured, error) {
	crds := &unstructured.UnstructuredList{}
	crds.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinitionList"))
	if err := c.List(ctx, crds, client.HasLabels{clusterctlv1.ClusterctlLabel}); err != nil {
		return nil, fmt.Errorf("list the CRDs that clusterctl labelled: %w", err)
	}
	kinds := []schema.GroupVersionKind{corev1.SchemeGroupVersion.WithKind("Secret"), corev1.SchemeGroupVersion.WithKind("ConfigMap"), controlPlaneKind, etcdKind}
	for _, item := range crds.Items {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, crd); err != nil {
			return nil, err
		}
		if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if version.Storage {
				kinds = append(kinds, schema.GroupVersionKind{Group: crd.Spec.Group, Version: version.Name, Kind: crd.Spec.Names.Kind})
			}
		}
	}
	var graph []*unstructured.Unstructured
	for _, kind := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("list %s: %w", kind, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			obj.SetGroupVersionKind(kind)
			if kind.Kind == "ConfigMap" && obj.GetName() == "kube-root-ca.crt" || kind.Kind == "Secret" && strings.Contains(obj.GetName(), "default-token") {
				continue
			}
			graph = append(graph, obj)
		}
	}
	slices.SortFunc(graph, func(a, b *unstructured.Unstructured) int {
		return strings.Compare(describe(a), describe(b))
	})
	for _, kind := range graphKinds {
		if !slices.ContainsFunc(graph, func(obj *unstructured.Unstructured) bool { return obj.GetKind() == kind }) {
			return nil, fmt.Errorf("the graph of %s holds no %s: the CRD of the kind lacks the label %s, or the object is not there", namespace, kind, clusterctlv1.ClusterctlLabel)
		}
	}
	return graph, nil
}

func describe(obj *unstructured.Unstructured) string {
	return obj.GetKind() + " " + obj.GetName()
}

func TestTheGraphHoldsTheLabelledKindsOfTheNamespaceAndTheSecretsAndConfigMapsAndLodiasTwo(t *testing.T) {
	q := newQuickStartNamespace()
	want := q.described()
	extension := clusterAPIKind("ExtensionConfig")
	unlabelled := schema.GroupVersionKind{Group: "unlabelled.example.com", Version: "v1", Kind: "Unlabelled"}
	q.add(
		crdOf(extension, apiextensionsv1.ClusterScoped, true),
		crdOf(unlabelled, apiextensionsv1.NamespaceScoped, false),
		object(unlabelled, "u"),
		object(extension, "e"),
		inOtherNamespace(object(corev1.SchemeGroupVersion.WithKind("Secret"), "of-another-namespace")),
		object(corev1.SchemeGroupVersion.WithKind("ConfigMap"), "kube-root-ca.crt"),
		object(corev1.SchemeGroupVersion.WithKind("Secret"), "default-token-abc"),
	)
	graph, err := listGraph(t.Context(), q.client(), quickStartKey.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, obj := range graph {
		got = append(got, describe(obj))
		if obj.GroupVersionKind().Kind == "" || obj.GroupVersionKind().Version == "" {
			t.Errorf("%s has the kind %s", describe(obj), obj.GroupVersionKind())
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the graph holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheGraphFailsWhenItHoldsNoObjectOfAKindTheSuiteCreated(t *testing.T) {
	for _, kind := range newQuickStartNamespace().kindsOfTheObjects("ConfigMap") {
		t.Run("no "+kind, func(t *testing.T) {
			q := newQuickStartNamespace()
			q.drop(kind)
			_, err := listGraph(t.Context(), q.client(), quickStartKey.Namespace)
			if err == nil || !strings.Contains(err.Error(), "holds no "+kind) {
				t.Fatalf("want an error that the graph holds no %s, got %v", kind, err)
			}
		})
	}
	for _, kind := range newQuickStartNamespace().kindsOfTheCRDs() {
		t.Run("the CRD of "+kind+" without the label", func(t *testing.T) {
			q := newQuickStartNamespace()
			q.unlabel(kind)
			_, err := listGraph(t.Context(), q.client(), quickStartKey.Namespace)
			if err == nil || !strings.Contains(err.Error(), "holds no "+kind) {
				t.Fatalf("want an error that the graph holds no %s, got %v", kind, err)
			}
		})
	}
}
