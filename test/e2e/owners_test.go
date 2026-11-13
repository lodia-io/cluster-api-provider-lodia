// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

const druidGroup = "druid.gardener.cloud"

var (
	ownedByCluster          = ownerRef(clusterAPIKind("Cluster"), "", false)
	controlledByDeployment  = ownerRef(clusterAPIKind("MachineDeployment"), "", true)
	controlledBySet         = ownerRef(clusterAPIKind("MachineSet"), "", true)
	controlledByMachine     = ownerRef(clusterAPIKind("Machine"), "", true)
	controlledByKubeadm     = ownerRef(bootstrapv1.GroupVersion.WithKind("KubeadmConfig"), "", true)
	controlledByProvider    = ownerRef(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), "", true)
	controlledByControlPlan = ownerRef(controlPlaneKind, "", true)
	controlledByEtcd        = ownerRef(etcdKind, "", true)
)

// expectedOwners are Cluster API's assertions for its own kinds, and the
// suite's for the provider's and Lodia's. An object may be owned in one of
// several ways; a kind without an assertion reports false.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/framework/ownerreference_helpers.go#L164-L357
func expectedOwners(cluster string, obj *unstructured.Unstructured) ([][]metav1.OwnerReference, bool) {
	switch obj.GetKind() {
	case "Cluster":
		return [][]metav1.OwnerReference{nil}, true
	case "LodiaControlPlane", "DevCluster", "DevMachineTemplate", "KubeadmConfigTemplate", "MachineDeployment":
		return [][]metav1.OwnerReference{{ownedByCluster}}, true
	case "MachineSet":
		return [][]metav1.OwnerReference{{controlledByDeployment}}, true
	case "Machine":
		return [][]metav1.OwnerReference{{controlledBySet}}, true
	case "DevMachine", "KubeadmConfig":
		return [][]metav1.OwnerReference{{controlledByMachine}}, true
	case "ControlPlane":
		return [][]metav1.OwnerReference{{controlledByProvider}}, true
	case "Etcd":
		return [][]metav1.OwnerReference{{controlledByControlPlan}}, true
	case "ConfigMap":
		return [][]metav1.OwnerReference{{controlledByControlPlan}, {controlledByEtcd}}, true
	case "Secret":
		if copied := obj.GetName() == secret.Name(cluster, secret.Kubeconfig) || obj.GetName() == secret.Name(cluster, secret.ClusterCA); copied {
			return [][]metav1.OwnerReference{{controlledByProvider}}, true
		}
		return [][]metav1.OwnerReference{{controlledByKubeadm}, {controlledByControlPlan}}, true
	}
	return nil, false
}

func ownersAre(got []metav1.OwnerReference, want []metav1.OwnerReference) bool {
	return slices.Equal(ownerStrings(got), ownerStrings(want))
}

func ownerStrings(refs []metav1.OwnerReference) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, fmt.Sprintf("%s/%s/%t", ref.APIVersion, ref.Kind, ref.Controller != nil && *ref.Controller))
	}
	slices.Sort(out)
	return out
}

func requireOwners(ctx context.Context, c client.Reader, key client.ObjectKey) error {
	graph, err := listGraph(ctx, c, key.Namespace)
	if err != nil {
		return err
	}
	uids := sets.New[types.UID]()
	for _, obj := range graph {
		uids.Insert(obj.GetUID())
	}
	var errs []error
	for _, obj := range graph {
		for _, ref := range obj.GetOwnerReferences() {
			if !uids.Has(ref.UID) {
				errs = append(errs, fmt.Errorf("%s: its owner %s %s is not an object of the namespace", describe(obj), ref.Kind, ref.Name))
			}
		}
		want, ok := expectedOwners(key.Name, obj)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: its kind does not have an associated ownerRef assertion", describe(obj)))
			continue
		}
		if !slices.ContainsFunc(want, func(set []metav1.OwnerReference) bool { return ownersAre(obj.GetOwnerReferences(), set) }) {
			var ways []string
			for _, set := range want {
				ways = append(ways, fmt.Sprint(ownerStrings(set)))
			}
			errs = append(errs, fmt.Errorf("unexpected ownerReferences for %s: wanted %s, actual %v", describe(obj), strings.Join(ways, " or "), ownerStrings(obj.GetOwnerReferences())))
		}
	}
	return errors.Join(errs...)
}

// ofClusterAPIOrTheProvider is what the removals under a pause touch: the
// kinds of Cluster API, its kubeadm bootstrap provider, CAPD and the provider,
// and the bootstrap data's Secret, which the kubeadm bootstrap provider puts its
// KubeadmConfig back on as the Secret's controller. Lodia's ControlPlane, the
// Etcd and every other Secret and ConfigMap are left alone: the provider never
// adopts a ControlPlane or a Secret that it does not control (docs/design.md,
// held by foreign_test.go and races_test.go), so it would not put an owner
// reference back, and Lodia and etcd-druid do not follow the Cluster's pause.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L289
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L1494-L1529
func ofClusterAPIOrTheProvider(obj *unstructured.Unstructured) bool {
	if owner := metav1.GetControllerOf(obj); owner != nil && owner.Kind == "KubeadmConfig" && ownerGroup(owner) == bootstrapv1.GroupVersion.Group {
		return true
	}
	switch obj.GroupVersionKind().Group {
	case "", lodiav1alpha1.GroupVersion.Group, druidGroup:
		return false
	}
	return true
}

func ownerGroup(owner *metav1.OwnerReference) string {
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil {
		return ""
	}
	return gv.Group
}

// withdraw nulls a metadata field with a merge patch, which no write to the
// object's status between the list and the patch can conflict with, and leaves
// an object that is gone since the list, as the garbage collector's last
// dependents of a deleted Machine may be. It stops at the first patch that
// fails, since a cluster that stopped answering would hold each of the others
// for its timeout.
func withdraw(ctx context.Context, c client.Client, graph []*unstructured.Unstructured, field string, held func(*unstructured.Unstructured) bool) error {
	patch := client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"metadata":{%q:null}}`, field))
	for _, obj := range graph {
		if !ofClusterAPIOrTheProvider(obj) || !held(obj) {
			continue
		}
		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(obj.GroupVersionKind())
		target.SetNamespace(obj.GetNamespace())
		target.SetName(obj.GetName())
		if err := client.IgnoreNotFound(c.Patch(ctx, target, patch)); err != nil {
			return fmt.Errorf("remove %s of %s: %w", field, describe(obj), err)
		}
	}
	return nil
}

func removeOwnerReferences(ctx context.Context, c client.Client, namespace string) error {
	graph, err := listGraph(ctx, c, namespace)
	if err != nil {
		return err
	}
	return withdraw(ctx, c, graph, "ownerReferences", func(obj *unstructured.Unstructured) bool { return len(obj.GetOwnerReferences()) > 0 })
}

func TestEveryObjectOfTheGraphHasTheOwnersItsKindWants(t *testing.T) {
	copied := secret.Name(quickStartKey.Name, secret.Kubeconfig)
	cases := []struct {
		name    string
		change  func(q *quickStartNamespace)
		failure string
	}{
		{"every object as it is when the Cluster is Available", func(*quickStartNamespace) {}, ""},
		{"the LodiaControlPlane without the Cluster", func(q *quickStartNamespace) {
			q.first("LodiaControlPlane").SetOwnerReferences(nil)
		}, "unexpected ownerReferences for LodiaControlPlane w"},
		{"Lodia's ControlPlane without the owner that the provider's apply writes", func(q *quickStartNamespace) {
			q.first("ControlPlane").SetOwnerReferences(nil)
		}, "unexpected ownerReferences for ControlPlane w"},
		{"Lodia's ControlPlane owned but not controlled by the LodiaControlPlane", func(q *quickStartNamespace) {
			ref := ownerRef(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), "w", false)
			q.first("ControlPlane").SetOwnerReferences([]metav1.OwnerReference{ref})
		}, "unexpected ownerReferences for ControlPlane w"},
		{"Lodia's ControlPlane controlled by the Cluster", func(q *quickStartNamespace) {
			q.first("ControlPlane").SetOwnerReferences([]metav1.OwnerReference{ownerRef(clusterAPIKind("Cluster"), "w", true)})
		}, "unexpected ownerReferences for ControlPlane w"},
		{"the Etcd without the ControlPlane", func(q *quickStartNamespace) { q.first("Etcd").SetOwnerReferences(nil) }, "unexpected ownerReferences for Etcd"},
		{"the kubeconfig copy without the LodiaControlPlane", func(q *quickStartNamespace) {
			q.get("Secret", copied).SetOwnerReferences(nil)
		}, "unexpected ownerReferences for Secret " + copied},
		{"the kubeconfig copy owned but not controlled by the LodiaControlPlane", func(q *quickStartNamespace) {
			q.get("Secret", copied).SetOwnerReferences([]metav1.OwnerReference{ownerRef(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), "w", false)})
		}, "unexpected ownerReferences for Secret " + copied},
		{"the kubeconfig copy controlled by Lodia's ControlPlane, which owns the other Secrets", func(q *quickStartNamespace) {
			q.get("Secret", copied).SetOwnerReferences([]metav1.OwnerReference{ownerRef(controlPlaneKind, "w", true)})
		}, "unexpected ownerReferences for Secret " + copied},
		{"the CA copy controlled by a KubeadmConfig", func(q *quickStartNamespace) {
			q.get("Secret", secret.Name(quickStartKey.Name, secret.ClusterCA)).SetOwnerReferences([]metav1.OwnerReference{ownerRef(bootstrapv1.GroupVersion.WithKind("KubeadmConfig"), "k", true)})
		}, "unexpected ownerReferences for Secret w-ca"},
		{"a Secret of Lodia's without an owner", func(q *quickStartNamespace) {
			q.get("Secret", lodiav1alpha1.AdminKubeconfigSecret.Name("w")).SetOwnerReferences(nil)
		}, "unexpected ownerReferences for Secret w-admin-kubeconfig"},
		{"a Secret of Lodia's controlled by the LodiaControlPlane", func(q *quickStartNamespace) {
			q.get("Secret", lodiav1alpha1.AdminKubeconfigSecret.Name("w")).SetOwnerReferences([]metav1.OwnerReference{ownerRef(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), "w", true)})
		}, "unexpected ownerReferences for Secret w-admin-kubeconfig"},
		{"the bootstrap data without the KubeadmConfig", func(q *quickStartNamespace) {
			q.get("Secret", "w-md-0-abc-1").SetOwnerReferences(nil)
		}, "unexpected ownerReferences for Secret w-md-0-abc-1"},
		{"a ConfigMap of Lodia's without an owner", func(q *quickStartNamespace) {
			q.get("ConfigMap", "w-egress-selector").SetOwnerReferences(nil)
		}, "unexpected ownerReferences for ConfigMap w-egress-selector"},
		{"a ConfigMap owned by a ClusterResourceSet", func(q *quickStartNamespace) {
			q.get("ConfigMap", "w-egress-selector").SetOwnerReferences([]metav1.OwnerReference{ownerRef(clusterAPIKind("ClusterResourceSet"), "c", false)})
		}, "unexpected ownerReferences for ConfigMap w-egress-selector"},
		{"a Machine owned by the MachineDeployment", func(q *quickStartNamespace) {
			q.first("Machine").SetOwnerReferences([]metav1.OwnerReference{ownerRef(clusterAPIKind("MachineDeployment"), "d", true)})
		}, "unexpected ownerReferences for Machine"},
		{"a Machine owned by its MachineSet and the Cluster", func(q *quickStartNamespace) {
			q.first("Machine").SetOwnerReferences(append(q.first("Machine").GetOwnerReferences(), ownerRef(clusterAPIKind("Cluster"), "w", false)))
		}, "unexpected ownerReferences for Machine"},
		{"a Machine controlled by a MachineSet of another version", func(q *quickStartNamespace) {
			ref := ownerRef(clusterAPIKind("MachineSet"), "s", true)
			ref.APIVersion = "cluster.x-k8s.io/v1alpha1"
			q.first("Machine").SetOwnerReferences([]metav1.OwnerReference{ref})
		}, "unexpected ownerReferences for Machine"},
		{"a MachineSet that the MachineDeployment owns without controlling", func(q *quickStartNamespace) {
			q.first("MachineSet").SetOwnerReferences([]metav1.OwnerReference{ownerRef(clusterAPIKind("MachineDeployment"), "d", false)})
		}, "unexpected ownerReferences for MachineSet"},
		{"the Cluster owned by something", func(q *quickStartNamespace) {
			q.first("Cluster").SetOwnerReferences([]metav1.OwnerReference{ownerRef(clusterAPIKind("ClusterClass"), "c", false)})
		}, "unexpected ownerReferences for Cluster w"},
		{"a DevMachine without its Machine", func(q *quickStartNamespace) { q.first("DevMachine").SetOwnerReferences(nil) }, "unexpected ownerReferences for DevMachine"},
		{"a template without the Cluster", func(q *quickStartNamespace) { q.first("KubeadmConfigTemplate").SetOwnerReferences(nil) }, "unexpected ownerReferences for KubeadmConfigTemplate"},
		{"a Machine owned by a MachineSet that is gone", func(q *quickStartNamespace) {
			q.first("Machine").SetOwnerReferences([]metav1.OwnerReference{ownerRef(clusterAPIKind("MachineSet"), "gone", true)})
		}, "Machine w-md-0-abc-1: its owner MachineSet gone is not an object of the namespace"},
		{"the bootstrap data of a KubeadmConfig that went with its deleted Machine", func(q *quickStartNamespace) {
			q.add(object(corev1.SchemeGroupVersion.WithKind("Secret"), "left", ownerRef(bootstrapv1.GroupVersion.WithKind("KubeadmConfig"), "deleted", true)))
		}, "Secret left: its owner KubeadmConfig deleted is not an object of the namespace"},
		{"a Machine owned by a MachineSet that was recreated under the same name", func(q *quickStartNamespace) {
			ref := ownerRef(clusterAPIKind("MachineSet"), "w-md-0-abc", true)
			ref.UID = "recreated"
			q.first("Machine").SetOwnerReferences([]metav1.OwnerReference{ref})
		}, "Machine w-md-0-abc-1: its owner MachineSet w-md-0-abc is not an object of the namespace"},
		{"an object of a kind that has no assertion", func(q *quickStartNamespace) {
			q.add(crdOf(clusterAPIKind("MachineHealthCheck"), apiextensionsv1.NamespaceScoped, true), object(clusterAPIKind("MachineHealthCheck"), "h", ownerRef(clusterAPIKind("Cluster"), "w", false)))
		}, "MachineHealthCheck h: its kind does not have an associated ownerRef assertion"},
		{"a Secret that is not Lodia's, Cluster API's or the provider's", func(q *quickStartNamespace) {
			q.add(object(corev1.SchemeGroupVersion.WithKind("Secret"), "stray", ownerRef(clusterAPIKind("Cluster"), "w", false)))
		}, "unexpected ownerReferences for Secret stray"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newQuickStartNamespace()
			c.change(q)
			requireFailure(t, requireOwners(t.Context(), q.client(), quickStartKey), c.failure)
		})
	}
}

func TestTheOwnerReferencesRemovedUnderAPauseAreTheLodiaControlPlanesAndClusterAPIsAndTheBootstrapDatas(t *testing.T) {
	removed := map[string]bool{
		"LodiaControlPlane": true, "DevCluster": true, "DevMachineTemplate": true, "KubeadmConfigTemplate": true,
		"MachineDeployment": true, "MachineSet": true, "Machine": true, "DevMachine": true, "KubeadmConfig": true,
		"Secret w-md-0-abc-1": true,
	}
	fresh := newQuickStartNamespace()
	fresh.add(object(corev1.SchemeGroupVersion.WithKind("Secret"), "of-another-kubeadm-config", ownerRef(schema.GroupVersionKind{Group: "other.example.com", Version: "v1", Kind: "KubeadmConfig"}, "k", true)))
	c := fresh.client()
	requireNoError(t, removeOwnerReferences(t.Context(), c, quickStartKey.Namespace))
	graph, err := listGraph(t.Context(), c, quickStartKey.Namespace)
	requireNoError(t, err)
	for _, obj := range graph {
		original := fresh.get(obj.GetKind(), obj.GetName())
		if removed[obj.GetKind()] || removed[describe(obj)] {
			if len(original.GetOwnerReferences()) == 0 {
				t.Fatalf("%s has no owner reference to remove", describe(obj))
			}
			if refs := obj.GetOwnerReferences(); len(refs) != 0 {
				t.Errorf("%s keeps %v", describe(obj), refs)
			}
			continue
		}
		if got, want := ownerStrings(obj.GetOwnerReferences()), ownerStrings(original.GetOwnerReferences()); !slices.Equal(got, want) {
			t.Errorf("%s has the owner references %v, want %v: the removal is not its to make", describe(obj), got, want)
		}
	}
}

func TestTheRemovalLeavesAnObjectThatIsGoneSinceTheList(t *testing.T) {
	q := newQuickStartNamespace()
	c := q.client()
	graph, err := listGraph(t.Context(), c, quickStartKey.Namespace)
	requireNoError(t, err)
	requireNoError(t, c.Delete(t.Context(), q.get("Machine", "w-md-0-abc-1")))
	requireNoError(t, c.Delete(t.Context(), q.get("KubeadmConfig", "w-md-0-abc-1")))
	requireNoError(t, withdraw(t.Context(), c, graph, "ownerReferences", func(obj *unstructured.Unstructured) bool { return len(obj.GetOwnerReferences()) > 0 }))
}

func TestTheRemovalStopsAtItsFirstFailure(t *testing.T) {
	q := newQuickStartNamespace()
	var patches int
	failing := interceptor.NewClient(q.client(), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			patches++
			return errors.New("the server does not answer")
		},
	})
	graph, err := listGraph(t.Context(), failing, quickStartKey.Namespace)
	requireNoError(t, err)
	err = withdraw(t.Context(), failing, graph, "ownerReferences", func(obj *unstructured.Unstructured) bool { return len(obj.GetOwnerReferences()) > 0 })
	requireFailure(t, err, "the server does not answer")
	if patches != 1 {
		t.Errorf("%d patches were tried, want the first alone", patches)
	}
}
