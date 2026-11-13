// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

var quickStartKey = client.ObjectKey{Namespace: "capi-join", Name: "w"}

func clusterAPIKind(kind string) schema.GroupVersionKind {
	return clusterv1.GroupVersion.WithKind(kind)
}

func devKind(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: kind}
}

func uidOf(kind, name string) types.UID { return types.UID("uid-of-" + kind + "-" + name) }

func ownerRef(gvk schema.GroupVersionKind, name string, controls bool) metav1.OwnerReference {
	ref := metav1.OwnerReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: name, UID: uidOf(gvk.Kind, name)}
	if controls {
		ref.Controller = new(true)
	}
	return ref
}

func object(gvk schema.GroupVersionKind, name string, owners ...metav1.OwnerReference) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(quickStartKey.Namespace)
	obj.SetName(name)
	obj.SetUID(uidOf(gvk.Kind, name))
	obj.SetOwnerReferences(owners)
	return obj
}

func withFinalizers(obj *unstructured.Unstructured, finalizers ...string) *unstructured.Unstructured {
	obj.SetFinalizers(finalizers)
	return obj
}

func inOtherNamespace(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetNamespace("other")
	return obj
}

func crdOf(kind schema.GroupVersionKind, scope apiextensionsv1.ResourceScope, labelled bool) *unstructured.Unstructured {
	crd := &apiextensionsv1.CustomResourceDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiextensionsv1.SchemeGroupVersion.String(), Kind: "CustomResourceDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(kind.Kind) + "s." + kind.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: kind.Group,
			Scope: scope,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind.Kind},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v0", Served: true},
				{Name: kind.Version, Served: true, Storage: true},
			},
		},
	}
	if labelled {
		crd.Labels = map[string]string{clusterctlv1.ClusterctlLabel: ""}
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(crd)
	if err != nil {
		panic(err)
	}
	return &unstructured.Unstructured{Object: obj}
}

type quickStartNamespace struct {
	objects []*unstructured.Unstructured
}

func newQuickStartNamespace() *quickStartNamespace {
	name := quickStartKey.Name
	cluster := ownerRef(clusterAPIKind("Cluster"), name, false)
	machine := ownerRef(clusterAPIKind("Machine"), name+"-md-0-abc-1", true)
	lcp := ownerRef(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), name, true)
	lodiaCP := ownerRef(controlPlaneKind, name, true)
	etcd := ownerRef(etcdKind, lodiav1alpha1.EtcdName(name), true)
	certificates := []string{name + "-etcd-ca-1", name + "-etcd-server-1"}
	controlPlane := withFinalizers(object(controlPlaneKind, name, lcp), lodiav1alpha1.DatastoreFinalizer)
	controlPlane.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: controller.FieldManager, Operation: metav1.ManagedFieldsOperationApply, APIVersion: controlPlaneKind.GroupVersion().String(), FieldsType: "FieldsV1", Time: &metav1.Time{Time: time.Unix(1000, 0)}, FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:version":{}}}`)},
		{Manager: "lodia", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: controlPlaneKind.GroupVersion().String(), FieldsType: "FieldsV1", Subresource: "status", Time: &metav1.Time{Time: time.Unix(1000, 0)}, FieldsV1: metav1.NewFieldsV1(`{"f:status":{}}`)},
	})
	druidEtcd := withFinalizers(object(etcdKind, lodiav1alpha1.EtcdName(name), lodiaCP), druidFinalizer)
	if err := unstructured.SetNestedField(druidEtcd.Object, map[string]any{
		"etcd": map[string]any{
			"clientUrlTls": map[string]any{
				"tlsCASecretRef":     map[string]any{"name": certificates[0]},
				"serverTLSSecretRef": map[string]any{"name": certificates[1]},
				"clientTLSSecretRef": map[string]any{"name": certificates[1]},
			},
		},
	}, "spec"); err != nil {
		panic(err)
	}
	secrets := corev1.SchemeGroupVersion.WithKind("Secret")
	configMaps := corev1.SchemeGroupVersion.WithKind("ConfigMap")
	q := &quickStartNamespace{}
	q.add(
		crdOf(clusterAPIKind("Cluster"), apiextensionsv1.NamespaceScoped, true),
		crdOf(clusterAPIKind("MachineDeployment"), apiextensionsv1.NamespaceScoped, true),
		crdOf(clusterAPIKind("MachineSet"), apiextensionsv1.NamespaceScoped, true),
		crdOf(clusterAPIKind("Machine"), apiextensionsv1.NamespaceScoped, true),
		crdOf(devKind("DevCluster"), apiextensionsv1.NamespaceScoped, true),
		crdOf(devKind("DevMachine"), apiextensionsv1.NamespaceScoped, true),
		crdOf(devKind("DevMachineTemplate"), apiextensionsv1.NamespaceScoped, true),
		crdOf(bootstrapv1.GroupVersion.WithKind("KubeadmConfig"), apiextensionsv1.NamespaceScoped, true),
		crdOf(bootstrapv1.GroupVersion.WithKind("KubeadmConfigTemplate"), apiextensionsv1.NamespaceScoped, true),
		crdOf(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), apiextensionsv1.NamespaceScoped, true),

		withFinalizers(object(clusterAPIKind("Cluster"), name), clusterv1.ClusterFinalizer),
		withFinalizers(object(v1alpha1.GroupVersion.WithKind("LodiaControlPlane"), name, cluster), v1alpha1.Finalizer),
		object(devKind("DevCluster"), name, cluster),
		object(devKind("DevMachineTemplate"), name+"-md-0", cluster),
		object(bootstrapv1.GroupVersion.WithKind("KubeadmConfigTemplate"), name+"-md-0", cluster),
		withFinalizers(object(clusterAPIKind("MachineDeployment"), name+"-md-0", cluster), clusterv1.MachineDeploymentFinalizer),
		withFinalizers(object(clusterAPIKind("MachineSet"), name+"-md-0-abc", ownerRef(clusterAPIKind("MachineDeployment"), name+"-md-0", true)), clusterv1.MachineSetFinalizer),
		withFinalizers(object(clusterAPIKind("Machine"), name+"-md-0-abc-1", ownerRef(clusterAPIKind("MachineSet"), name+"-md-0-abc", true)), clusterv1.MachineFinalizer),
		withFinalizers(object(devKind("DevMachine"), name+"-md-0-abc-1", machine), devMachineFinalizer),
		object(bootstrapv1.GroupVersion.WithKind("KubeadmConfig"), name+"-md-0-abc-1", machine),
		controlPlane,
		druidEtcd,

		object(secrets, secret.Name(name, secret.Kubeconfig), lcp),
		object(secrets, secret.Name(name, secret.ClusterCA), lcp),
		object(secrets, name+"-md-0-abc-1", ownerRef(bootstrapv1.GroupVersion.WithKind("KubeadmConfig"), name+"-md-0-abc-1", true)),
		withFinalizers(object(secrets, certificates[0], lodiaCP), druidFinalizer),
		withFinalizers(object(secrets, certificates[1], lodiaCP), druidFinalizer),
		object(secrets, lodiav1alpha1.AdminKubeconfigSecret.Name(name), lodiaCP),
		object(configMaps, name+"-egress-selector", lodiaCP),
		object(configMaps, name+"-etcd-config", etcd),
	)
	return q
}

func (q *quickStartNamespace) add(objects ...*unstructured.Unstructured) {
	q.objects = append(q.objects, objects...)
}

func (q *quickStartNamespace) isCRD(obj *unstructured.Unstructured) bool {
	return obj.GetKind() == "CustomResourceDefinition"
}

func (q *quickStartNamespace) get(kind, name string) *unstructured.Unstructured {
	for _, obj := range q.objects {
		if obj.GetKind() == kind && obj.GetName() == name && !q.isCRD(obj) {
			return obj
		}
	}
	panic("no " + kind + " " + name)
}

func (q *quickStartNamespace) first(kind string) *unstructured.Unstructured {
	for _, obj := range q.objects {
		if obj.GetKind() == kind && !q.isCRD(obj) {
			return obj
		}
	}
	panic("no " + kind)
}

func (q *quickStartNamespace) drop(kind string) {
	q.objects = slices.DeleteFunc(q.objects, func(obj *unstructured.Unstructured) bool { return obj.GetKind() == kind && !q.isCRD(obj) })
}

func (q *quickStartNamespace) unlabel(kind string) {
	for _, obj := range q.objects {
		if name, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind"); q.isCRD(obj) && name == kind {
			obj.SetLabels(nil)
		}
	}
}

func (q *quickStartNamespace) kindsOfTheCRDs() []string {
	var kinds []string
	for _, obj := range q.objects {
		if name, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind"); q.isCRD(obj) {
			kinds = append(kinds, name)
		}
	}
	return kinds
}

func (q *quickStartNamespace) kindsOfTheObjects(except ...string) []string {
	var kinds []string
	for _, obj := range q.objects {
		if kind := obj.GetKind(); !q.isCRD(obj) && !slices.Contains(except, kind) && !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func (q *quickStartNamespace) described() []string {
	var all []string
	for _, obj := range q.objects {
		if !q.isCRD(obj) {
			all = append(all, describe(obj))
		}
	}
	slices.Sort(all)
	return all
}

func (q *quickStartNamespace) client() client.WithWatch {
	var objects []client.Object
	for _, obj := range q.objects {
		objects = append(objects, obj.DeepCopy())
	}
	return fake.NewClientBuilder().WithScheme(controller.Scheme()).WithReturnManagedFields().WithObjects(objects...).Build()
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
