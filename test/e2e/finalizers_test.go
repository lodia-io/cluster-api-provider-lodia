// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

const (
	// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/api/v1beta2/devmachine_types.go#L28
	devMachineFinalizer = "dockermachine.infrastructure.cluster.x-k8s.io"
	// https://github.com/gardener/etcd-druid/blob/v0.39.0/api/common/constants.go#L9
	druidFinalizer = "druid.gardener.cloud/etcd-druid"
)

// etcdSecrets are the Secrets that etcd-druid holds with its finalizer.
// https://github.com/gardener/etcd-druid/blob/v0.39.0/internal/controller/secret/reconciler.go#L71-L100
func etcdSecrets(graph []*unstructured.Unstructured) sets.Set[string] {
	names := sets.New[string]()
	for _, obj := range graph {
		if obj.GroupVersionKind() != etcdKind {
			continue
		}
		for _, path := range [][]string{
			{"spec", "etcd", "clientUrlTls", "tlsCASecretRef"}, {"spec", "etcd", "clientUrlTls", "serverTLSSecretRef"}, {"spec", "etcd", "clientUrlTls", "clientTLSSecretRef"},
			{"spec", "etcd", "peerUrlTls", "tlsCASecretRef"}, {"spec", "etcd", "peerUrlTls", "serverTLSSecretRef"},
			{"spec", "backup", "tls", "tlsCASecretRef"}, {"spec", "backup", "tls", "serverTLSSecretRef"}, {"spec", "backup", "tls", "clientTLSSecretRef"},
			{"spec", "backup", "store", "secretRef"},
		} {
			if name, _, _ := unstructured.NestedString(obj.Object, append(path, "name")...); name != "" {
				names.Insert(name)
			}
		}
	}
	return names
}

// expectedFinalizers are all the finalizers of an object, as Cluster API's
// check compares them, an empty set for a kind without an assertion. The
// DevCluster has none because it is externally managed.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/framework/finalizers_helpers.go#L123-L153
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/reconcilers/devcluster_reconciler.go#L103-L107
func expectedFinalizers(obj *unstructured.Unstructured, referenced sets.Set[string]) []string {
	switch obj.GetKind() {
	case "Cluster":
		return []string{clusterv1.ClusterFinalizer}
	case "LodiaControlPlane":
		return []string{v1alpha1.Finalizer}
	case "MachineDeployment":
		return []string{clusterv1.MachineDeploymentFinalizer}
	case "MachineSet":
		return []string{clusterv1.MachineSetFinalizer}
	case "Machine":
		return []string{clusterv1.MachineFinalizer}
	case "DevMachine":
		return []string{devMachineFinalizer}
	case "ControlPlane":
		return []string{lodiav1alpha1.DatastoreFinalizer}
	case "Etcd":
		return []string{druidFinalizer}
	case "Secret":
		if referenced.Has(obj.GetName()) {
			return []string{druidFinalizer}
		}
	}
	return nil
}

func requireFinalizers(ctx context.Context, c client.Reader, key client.ObjectKey) error {
	graph, err := listGraph(ctx, c, key.Namespace)
	if err != nil {
		return err
	}
	referenced := etcdSecrets(graph)
	var errs []error
	for _, obj := range graph {
		want, got := sets.New(expectedFinalizers(obj, referenced)...), sets.New(obj.GetFinalizers()...)
		if !want.Equal(got) {
			errs = append(errs, fmt.Errorf("unexpected finalizers for %s: expected %v, found %v", describe(obj), sets.List(want), sets.List(got)))
		}
	}
	return errors.Join(errs...)
}

func removeFinalizers(ctx context.Context, c client.Client, namespace string) error {
	graph, err := listGraph(ctx, c, namespace)
	if err != nil {
		return err
	}
	return withdraw(ctx, c, graph, "finalizers", func(obj *unstructured.Unstructured) bool { return len(obj.GetFinalizers()) > 0 })
}

func TestEveryObjectOfTheGraphHoldsExactlyTheFinalizersItsKindWants(t *testing.T) {
	cases := []struct {
		name    string
		change  func(q *quickStartNamespace)
		failure string
	}{
		{"every object as it is when the Cluster is Available", func(*quickStartNamespace) {}, ""},
		{"the LodiaControlPlane without the provider's finalizer", func(q *quickStartNamespace) {
			q.first("LodiaControlPlane").SetFinalizers(nil)
		}, "unexpected finalizers for LodiaControlPlane w: expected [controlplane.lodia.io/lodiacontrolplane], found []"},
		{"the LodiaControlPlane with another finalizer beside the provider's", func(q *quickStartNamespace) {
			q.first("LodiaControlPlane").SetFinalizers([]string{v1alpha1.Finalizer, "other"})
		}, "unexpected finalizers for LodiaControlPlane w"},
		{"a Machine with a finalizer beside its own", func(q *quickStartNamespace) {
			q.first("Machine").SetFinalizers([]string{clusterv1.MachineFinalizer, "other"})
		}, "unexpected finalizers for Machine"},
		{"a Machine without its finalizer", func(q *quickStartNamespace) { q.first("Machine").SetFinalizers(nil) }, "unexpected finalizers for Machine"},
		{"a Machine with another one's finalizer", func(q *quickStartNamespace) {
			q.first("Machine").SetFinalizers([]string{clusterv1.MachineSetFinalizer})
		}, "unexpected finalizers for Machine"},
		{"the Cluster without its finalizer", func(q *quickStartNamespace) { q.first("Cluster").SetFinalizers(nil) }, "unexpected finalizers for Cluster w"},
		{"the MachineDeployment without its finalizer", func(q *quickStartNamespace) { q.first("MachineDeployment").SetFinalizers(nil) }, "unexpected finalizers for MachineDeployment"},
		{"the MachineSet without its finalizer", func(q *quickStartNamespace) { q.first("MachineSet").SetFinalizers(nil) }, "unexpected finalizers for MachineSet"},
		{"the DevMachine without its finalizer", func(q *quickStartNamespace) { q.first("DevMachine").SetFinalizers(nil) }, "unexpected finalizers for DevMachine"},
		{"the externally managed DevCluster with a finalizer", func(q *quickStartNamespace) {
			q.first("DevCluster").SetFinalizers([]string{"dockercluster.infrastructure.cluster.x-k8s.io"})
		}, "unexpected finalizers for DevCluster w"},
		{"a KubeadmConfig with a finalizer", func(q *quickStartNamespace) { q.first("KubeadmConfig").SetFinalizers([]string{"other"}) }, "unexpected finalizers for KubeadmConfig"},
		{"Lodia's ControlPlane without the Datastore controller's finalizer", func(q *quickStartNamespace) {
			q.first("ControlPlane").SetFinalizers(nil)
		}, "unexpected finalizers for ControlPlane w: expected [lodia.io/datastore], found []"},
		{"Lodia's ControlPlane with a finalizer of the provider's beside it", func(q *quickStartNamespace) {
			q.first("ControlPlane").SetFinalizers([]string{lodiav1alpha1.DatastoreFinalizer, v1alpha1.Finalizer})
		}, "unexpected finalizers for ControlPlane w"},
		{"the Etcd without etcd-druid's finalizer", func(q *quickStartNamespace) { q.first("Etcd").SetFinalizers(nil) }, "unexpected finalizers for Etcd"},
		{"a Secret that the Etcd references without etcd-druid's finalizer", func(q *quickStartNamespace) {
			q.get("Secret", "w-etcd-ca-1").SetFinalizers(nil)
		}, "unexpected finalizers for Secret w-etcd-ca-1: expected [druid.gardener.cloud/etcd-druid], found []"},
		{"a Secret that the Etcd does not reference with etcd-druid's finalizer", func(q *quickStartNamespace) {
			q.get("Secret", lodiav1alpha1.AdminKubeconfigSecret.Name("w")).SetFinalizers([]string{druidFinalizer})
		}, "unexpected finalizers for Secret w-admin-kubeconfig"},
		{"a copy that the provider holds with a finalizer", func(q *quickStartNamespace) {
			q.get("Secret", "w-kubeconfig").SetFinalizers([]string{v1alpha1.Finalizer})
		}, "unexpected finalizers for Secret w-kubeconfig"},
		{"a ConfigMap with a finalizer", func(q *quickStartNamespace) { q.first("ConfigMap").SetFinalizers([]string{"other"}) }, "unexpected finalizers for ConfigMap"},
		{"a template with a finalizer", func(q *quickStartNamespace) { q.first("DevMachineTemplate").SetFinalizers([]string{"other"}) }, "unexpected finalizers for DevMachineTemplate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newQuickStartNamespace()
			c.change(q)
			requireFailure(t, requireFinalizers(t.Context(), q.client(), quickStartKey), c.failure)
		})
	}
}

func TestTheSecretsAnEtcdReferencesAreTheOnesItsSpecNames(t *testing.T) {
	etcd := object(etcdKind, "e")
	if err := unstructured.SetNestedField(etcd.Object, map[string]any{
		"etcd": map[string]any{
			"clientUrlTls": map[string]any{
				"tlsCASecretRef": map[string]any{"name": "client-ca"}, "serverTLSSecretRef": map[string]any{"name": "client-server"}, "clientTLSSecretRef": map[string]any{"name": "client-client"},
			},
			"peerUrlTls": map[string]any{"tlsCASecretRef": map[string]any{"name": "peer-ca"}, "serverTLSSecretRef": map[string]any{"name": "peer-server"}},
		},
		"backup": map[string]any{
			"tls":   map[string]any{"tlsCASecretRef": map[string]any{"name": "backup-ca"}, "serverTLSSecretRef": map[string]any{"name": "backup-server"}, "clientTLSSecretRef": map[string]any{"name": ""}},
			"store": map[string]any{"secretRef": map[string]any{"name": "store"}},
		},
		"labels": map[string]any{"name": "not-a-secret"},
	}, "spec"); err != nil {
		t.Fatal(err)
	}
	got := sets.List(etcdSecrets([]*unstructured.Unstructured{etcd, object(controlPlaneKind, "c")}))
	want := []string{"backup-ca", "backup-server", "client-ca", "client-client", "client-server", "peer-ca", "peer-server", "store"}
	if !slices.Equal(got, want) {
		t.Fatalf("the Etcd references %v, want %v", got, want)
	}
}

func TestTheFinalizersRemovedUnderAPauseAreTheLodiaControlPlanesAndClusterAPIsAlone(t *testing.T) {
	removed := map[string]bool{
		"Cluster": true, "LodiaControlPlane": true, "MachineDeployment": true, "MachineSet": true, "Machine": true, "DevMachine": true,
	}
	fresh := newQuickStartNamespace()
	c := fresh.client()
	requireNoError(t, removeFinalizers(t.Context(), c, quickStartKey.Namespace))
	graph, err := listGraph(t.Context(), c, quickStartKey.Namespace)
	requireNoError(t, err)
	for _, obj := range graph {
		original := fresh.get(obj.GetKind(), obj.GetName())
		if removed[obj.GetKind()] {
			if len(original.GetFinalizers()) == 0 {
				t.Fatalf("%s has no finalizer to remove", describe(obj))
			}
			if got := obj.GetFinalizers(); len(got) != 0 {
				t.Errorf("%s keeps %v", describe(obj), got)
			}
			continue
		}
		if got, want := obj.GetFinalizers(), original.GetFinalizers(); !slices.Equal(got, want) {
			t.Errorf("%s has the finalizers %v, want %v: the removal is not its to make", describe(obj), got, want)
		}
	}
}
