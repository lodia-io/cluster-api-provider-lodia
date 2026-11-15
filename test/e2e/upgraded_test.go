// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

const (
	providerDeployment = "cluster-api-provider-lodia-controller-manager"
	grantBinding       = "cluster-api-provider-lodia-capi-kubeadm-bootstrap-lodia-controlplane"
	providerInventory  = "control-plane-lodia"
)

var (
	inventoryKind = schema.GroupVersionKind{Group: "clusterctl.cluster.x-k8s.io", Version: "v1alpha3", Kind: "Provider"}
	managerKey    = client.ObjectKey{Namespace: providerNamespace, Name: providerDeployment}
)

type beforeTheUpgrade struct {
	deployment, binding types.UID
	made                map[string]types.UID
	worker              *clusterv1.Machine
	available           map[string]metav1.Time
}

func managerImageIn(components []byte) (string, error) {
	var images []string
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(components), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return "", err
		}
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			if container, ok := c.(map[string]any); ok && container["name"] == "manager" {
				image, _ := container["image"].(string)
				images = append(images, image)
			}
		}
	}
	if len(images) != 1 {
		return "", fmt.Errorf("the components have %d manager containers, want 1", len(images))
	}
	return images[0], nil
}

func requireProviderAt(ctx context.Context, k8s client.Reader, version, image string) (*appsv1.Deployment, error) {
	inventory := &unstructured.Unstructured{}
	inventory.SetGroupVersionKind(inventoryKind)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: providerNamespace, Name: providerInventory}, inventory); err != nil {
		return nil, err
	}
	if got, _, _ := unstructured.NestedString(inventory.Object, "version"); got != version {
		return nil, fmt.Errorf("clusterctl's inventory says the provider is at %q, want %s", got, version)
	}
	deployment := &appsv1.Deployment{}
	if err := k8s.Get(ctx, managerKey, deployment); err != nil {
		return nil, err
	}
	var images []string
	for _, c := range deployment.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			images = append(images, c.Image)
		}
	}
	if !slices.Equal(images, []string{image}) {
		return nil, fmt.Errorf("the manager of Deployment %s runs %v, want %s", deployment.Name, images, image)
	}
	return deployment, nil
}

func whatTheProviderMade(ctx context.Context, k8s client.Reader, key client.ObjectKey) (map[string]types.UID, error) {
	made := make(map[string]types.UID)
	for _, obj := range []client.Object{
		&v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
		&lodiav1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.ClusterCA)}},
	} {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return nil, err
		}
		made[fmt.Sprintf("%T %s", obj, obj.GetName())] = obj.GetUID()
	}
	return made, nil
}

func availableSince(what string, conditions []metav1.Condition) (metav1.Time, error) {
	available := meta.FindStatusCondition(conditions, clusterv1.AvailableCondition)
	if available == nil || available.Status != metav1.ConditionTrue {
		return metav1.Time{}, fmt.Errorf("%s: Available is %+v", what, available)
	}
	return available.LastTransitionTime, nil
}

func readBeforeTheUpgrade(ctx context.Context, k8s client.Reader, key client.ObjectKey) (*beforeTheUpgrade, error) {
	before := &beforeTheUpgrade{available: make(map[string]metav1.Time)}
	deployment := &appsv1.Deployment{}
	if err := k8s.Get(ctx, managerKey, deployment); err != nil {
		return nil, err
	}
	binding := &rbacv1.ClusterRoleBinding{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: grantBinding}, binding); err != nil {
		return nil, err
	}
	before.deployment, before.binding = deployment.UID, binding.UID
	made, err := whatTheProviderMade(ctx, k8s, key)
	if err != nil {
		return nil, err
	}
	before.made = made
	workers, err := runningWorkers(ctx, k8s, key, 1)
	if err != nil {
		return nil, err
	}
	before.worker = &workers[0]
	cluster, lcp, err := readBoth(ctx, k8s, key)
	if err != nil {
		return nil, err
	}
	for what, conditions := range map[string][]metav1.Condition{"Cluster": cluster.Status.Conditions, "LodiaControlPlane": lcp.Status.Conditions} {
		since, err := availableSince(what, conditions)
		if err != nil {
			return nil, err
		}
		before.available[what] = since
	}
	return before, nil
}

func requireTheProviderReplaced(ctx context.Context, k8s client.Reader, before *beforeTheUpgrade, version, image string) error {
	deployment, err := requireProviderAt(ctx, k8s, version, image)
	if err != nil {
		return err
	}
	if deployment.UID == before.deployment {
		return fmt.Errorf("Deployment %s has the UID of before the upgrade, %s: clusterctl deletes and creates it", deployment.Name, deployment.UID)
	}
	return nil
}

func requireTheGrantKept(ctx context.Context, k8s client.Reader, before *beforeTheUpgrade) error {
	binding := &rbacv1.ClusterRoleBinding{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: grantBinding}, binding); err != nil {
		return err
	}
	if binding.UID != before.binding {
		return fmt.Errorf("ClusterRoleBinding %s has the UID %s, before the upgrade %s: clusterctl keeps it and patches it", binding.Name, binding.UID, before.binding)
	}
	return nil
}

// A pod's host name is its name, and the holder of a Lease that the manager
// takes is the host name and a UUID:
// https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.2/pkg/leaderelection/leader_election.go#L93-L97
func requireTheNewManagerLeads(ctx context.Context, k8s client.Reader) error {
	lease := &coordinationv1.Lease{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: providerNamespace, Name: controller.LeaderElectionID}, lease); err != nil {
		return err
	}
	holder := ptr.Deref(lease.Spec.HolderIdentity, "")
	podName, _, ok := strings.Cut(holder, "_")
	if !ok || podName == "" {
		return fmt.Errorf("Lease %s has the holder %q, want a pod's name and a UUID", lease.Name, holder)
	}
	deployment := &appsv1.Deployment{}
	if err := k8s.Get(ctx, managerKey, deployment); err != nil {
		return err
	}
	pod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: providerNamespace, Name: podName}, pod); err != nil {
		return fmt.Errorf("Lease %s is held by %q: %w", lease.Name, holder, err)
	}
	set := metav1.GetControllerOf(pod)
	if set == nil || set.Kind != "ReplicaSet" {
		return fmt.Errorf("Lease %s is held by Pod %s, which no ReplicaSet controls", lease.Name, pod.Name)
	}
	replicas := &appsv1.ReplicaSet{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: providerNamespace, Name: set.Name}, replicas); err != nil {
		return fmt.Errorf("Lease %s is held by Pod %s of ReplicaSet %s: %w", lease.Name, pod.Name, set.Name, err)
	}
	if owner := metav1.GetControllerOf(replicas); owner == nil || owner.UID != deployment.UID {
		return fmt.Errorf("Lease %s is held by Pod %s, whose ReplicaSet %s does not belong to Deployment %s of UID %s", lease.Name, pod.Name, replicas.Name, deployment.Name, deployment.UID)
	}
	return nil
}

func requireWhatTheReleaseMadeKept(ctx context.Context, k8s client.Reader, key client.ObjectKey, before *beforeTheUpgrade) error {
	made, err := whatTheProviderMade(ctx, k8s, key)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(before.made)) {
		if made[name] != before.made[name] {
			errs = append(errs, fmt.Errorf("%s has the UID %q, before the upgrade %q: the new manager made another", name, made[name], before.made[name]))
		}
	}
	cluster, lcp, err := readBoth(ctx, k8s, key)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for what, conditions := range map[string][]metav1.Condition{"Cluster": cluster.Status.Conditions, "LodiaControlPlane": lcp.Status.Conditions} {
		since, err := availableSince(what, conditions)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if was := before.available[what]; !since.Equal(&was) {
			errs = append(errs, fmt.Errorf("the Available of the %s changed at %s, before the upgrade it had changed at %s: it did not stay True through the upgrade", what, since, was))
		}
	}
	for _, condition := range []string{clusterv1.ReadyCondition, clusterv1.AvailableCondition} {
		if c := meta.FindStatusCondition(lcp.Status.Conditions, condition); c != nil && c.Reason == v1alpha1.CopyOfAnotherObjectReason {
			errs = append(errs, fmt.Errorf("the %s of LodiaControlPlane %s is %s: the new manager reads the object as a copy of another", condition, lcp.Name, c.Reason))
		}
	}
	return errors.Join(append(errs, requireCopies(ctx, k8s, key, lcp))...)
}

func TestTheImageOfTheManagerIsTheOneContainerNamedManager(t *testing.T) {
	const deployment = "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n"
	cases := []struct {
		name, components, want, failure string
	}{
		{"one manager", deployment + "      - name: manager\n        image: example.io/m:v1@sha256:aa\n", "example.io/m:v1@sha256:aa", ""},
		{"a manager beside a sidecar", deployment + "      - name: sidecar\n        image: example.io/s:v1\n      - name: manager\n        image: example.io/m:v1\n", "example.io/m:v1", ""},
		{"a manager in the second document", "kind: Namespace\n---\n" + deployment + "      - name: manager\n        image: example.io/m:v2\n", "example.io/m:v2", ""},
		{"a container of another name", deployment + "      - name: other\n        image: example.io/o:v1\n", "", "0 manager containers"},
		{"a manager of another kind", strings.Replace(deployment, "Deployment", "StatefulSet", 1) + "      - name: manager\n        image: example.io/m:v1\n", "", "0 manager containers"},
		{"two managers", deployment + "      - name: manager\n        image: example.io/m:v1\n---\n" + deployment + "      - name: manager\n        image: example.io/m:v2\n", "", "2 manager containers"},
		{"text that is not YAML", "a: [\n", "", "yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := managerImageIn([]byte(c.components))
			switch {
			case c.failure == "" && (err != nil || got != c.want):
				t.Fatalf("got %q and %v, want %q", got, err, c.want)
			case c.failure != "" && (err == nil || !strings.Contains(err.Error(), c.failure)):
				t.Fatalf("want an error with %q, got %q and %v", c.failure, got, err)
			}
		})
	}
}

const (
	releaseImage = "ghcr.io/lodia-io/cluster-api-provider-lodia:v0.1.0-alpha.2@sha256:aa"
	treeImage    = "kind.local/cluster-api-provider-lodia:latest@sha256:bb"
	treeVersion  = "v0.1.99"
)

func inventoryAt(version string) *unstructured.Unstructured {
	inventory := &unstructured.Unstructured{Object: map[string]any{"version": version}}
	inventory.SetGroupVersionKind(inventoryKind)
	inventory.SetNamespace(providerNamespace)
	inventory.SetName(providerInventory)
	return inventory
}

func managerDeployment(uid types.UID, image string) *appsv1.Deployment {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: providerNamespace, Name: providerDeployment, UID: uid}}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar", Image: "example.io/s:v1"}, {Name: "manager", Image: image}}
	return deployment
}

func readerOf(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(controller.Scheme()).WithObjects(objs...).Build()
}

func TestTheProviderIsReplacedWhenTheInventoryAndTheDeploymentSayTheNewReleaseAndTheUIDIsNew(t *testing.T) {
	before := &beforeTheUpgrade{deployment: "old"}
	cases := []struct {
		name    string
		objs    []client.Object
		failure string
	}{
		{"upgraded", []client.Object{inventoryAt(treeVersion), managerDeployment("new", treeImage)}, ""},
		{"the inventory still says the release", []client.Object{inventoryAt("v0.1.0-alpha.2"), managerDeployment("new", treeImage)}, `says the provider is at "v0.1.0-alpha.2"`},
		{"no inventory", []client.Object{managerDeployment("new", treeImage)}, "not found"},
		{"the Deployment still runs the release's image", []client.Object{inventoryAt(treeVersion), managerDeployment("new", releaseImage)}, "runs [" + releaseImage + "]"},
		{"the Deployment is the one of before", []client.Object{inventoryAt(treeVersion), managerDeployment("old", treeImage)}, "has the UID of before the upgrade"},
		{"no Deployment", []client.Object{inventoryAt(treeVersion)}, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireFailure(t, requireTheProviderReplaced(t.Context(), readerOf(t, c.objs...), before, treeVersion, treeImage), c.failure)
		})
	}
}

func TestTheProviderRunsTheReleaseBeforeTheUpgrade(t *testing.T) {
	cases := []struct {
		name    string
		objs    []client.Object
		failure string
	}{
		{"the release", []client.Object{inventoryAt("v0.1.0-alpha.2"), managerDeployment("old", releaseImage)}, ""},
		{"the tree's build", []client.Object{inventoryAt(treeVersion), managerDeployment("old", treeImage)}, `says the provider is at "v0.1.99"`},
		{"the tree's image under the release's version", []client.Object{inventoryAt("v0.1.0-alpha.2"), managerDeployment("old", treeImage)}, "runs [" + treeImage + "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := requireProviderAt(t.Context(), readerOf(t, c.objs...), "v0.1.0-alpha.2", releaseImage)
			requireFailure(t, err, c.failure)
		})
	}
}

func TestTheGrantIsKeptWhenItsBindingHasTheUIDOfBeforeTheUpgrade(t *testing.T) {
	binding := func(uid types.UID) *rbacv1.ClusterRoleBinding {
		return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: grantBinding, UID: uid}}
	}
	before := &beforeTheUpgrade{binding: "b"}
	requireFailure(t, requireTheGrantKept(t.Context(), readerOf(t, binding("b")), before), "")
	requireFailure(t, requireTheGrantKept(t.Context(), readerOf(t, binding("recreated")), before), "has the UID recreated, before the upgrade b")
	requireFailure(t, requireTheGrantKept(t.Context(), readerOf(t), before), "not found")
}

type leadership struct {
	holder     *string
	pod        *corev1.Pod
	replicaSet *appsv1.ReplicaSet
	deployment *appsv1.Deployment
}

func (l leadership) reader(t *testing.T) client.Client {
	t.Helper()
	objs := []client.Object{&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: providerNamespace, Name: "controlplane.lodia.io"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: l.holder},
	}}
	if l.pod != nil {
		objs = append(objs, l.pod)
	}
	if l.replicaSet != nil {
		objs = append(objs, l.replicaSet)
	}
	if l.deployment != nil {
		objs = append(objs, l.deployment)
	}
	return readerOf(t, objs...)
}

func newLeadership() leadership {
	deployment := managerDeployment("new", treeImage)
	set := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: providerNamespace, Name: "manager-new", UID: "new-set"}}
	set.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: providerNamespace, Name: "manager-new-abc"}}
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(set, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
	return leadership{holder: new("manager-new-abc_8f4bd1b4-0000-4000-8000-000000000000"), pod: pod, replicaSet: set, deployment: deployment}
}

func TestTheNewManagerLeadsWhenTheHolderOfTheLeaseIsAPodOfTheNewDeployment(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*leadership)
		failure string
	}{
		{"a pod of the new Deployment holds the Lease", func(*leadership) {}, ""},
		{"no holder", func(l *leadership) { l.holder = nil }, `has the holder ""`},
		{"a holder that is no pod name and UUID", func(l *leadership) { l.holder = new("manager-new-abc") }, `has the holder "manager-new-abc"`},
		{"a holder without a pod name", func(l *leadership) { l.holder = new("_uuid") }, `has the holder "_uuid"`},
		{"the pod of the old manager, gone", func(l *leadership) { l.holder = new("manager-old-xyz_uuid") }, `is held by "manager-old-xyz_uuid"`},
		{"a pod that no ReplicaSet controls", func(l *leadership) { l.pod.OwnerReferences = nil }, "which no ReplicaSet controls"},
		{"a pod that another kind controls", func(l *leadership) { l.pod.OwnerReferences[0].Kind = "StatefulSet" }, "which no ReplicaSet controls"},
		{"a pod whose ReplicaSet is gone", func(l *leadership) { l.replicaSet = nil }, "of ReplicaSet manager-new"},
		{"a ReplicaSet of the old Deployment", func(l *leadership) { l.replicaSet.OwnerReferences[0].UID = "old" }, "does not belong to Deployment"},
		{"a ReplicaSet that no Deployment controls", func(l *leadership) { l.replicaSet.OwnerReferences = nil }, "does not belong to Deployment"},
		{"no Deployment", func(l *leadership) { l.deployment = nil }, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLeadership()
			c.change(&l)
			requireFailure(t, requireTheNewManagerLeads(t.Context(), l.reader(t)), c.failure)
		})
	}
}

type upgraded struct {
	cluster          *clusterv1.Cluster
	lcp              *v1alpha1.LodiaControlPlane
	controlPlane     *lodiav1alpha1.ControlPlane
	secrets          map[string]*corev1.Secret
	machines         []*clusterv1.Machine
	deployment       *appsv1.Deployment
	binding          *rbacv1.ClusterRoleBinding
	bindingIsMissing bool
}

var (
	madeAt        = metav1.NewTime(time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC))
	madeAgainAt   = metav1.NewTime(time.Date(2026, 11, 15, 1, 5, 0, 0, time.UTC))
	upgradedKey   = scaleKey
	upgradedNames = map[string]string{
		"kubeconfig-source": lodiav1alpha1.AdminKubeconfigSecret.Name(upgradedKey.Name), "ca-source": lodiav1alpha1.CABundleSecret.Name(upgradedKey.Name),
		"kubeconfig-copy": secret.Name(upgradedKey.Name, secret.Kubeconfig), "ca-copy": secret.Name(upgradedKey.Name, secret.ClusterCA),
	}
)

func newUpgraded() *upgraded {
	meta := metav1.ObjectMeta{Namespace: upgradedKey.Namespace, Name: upgradedKey.Name}
	cluster := &clusterv1.Cluster{ObjectMeta: meta}
	cluster.UID = "cluster"
	cluster.Status.Conditions = []metav1.Condition{{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: madeAt}}
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: meta}
	lcp.UID = "lcp"
	lcp.Status.Conditions = []metav1.Condition{
		{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: "Reachable", LastTransitionTime: madeAt},
		{Type: clusterv1.ReadyCondition, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: madeAt},
	}
	controlPlane := &lodiav1alpha1.ControlPlane{ObjectMeta: meta}
	controlPlane.UID = "control-plane"
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(lcp, v1alpha1.GroupVersion.WithKind("LodiaControlPlane"))}
	secrets := map[string]*corev1.Secret{
		"kubeconfig-source": {Data: map[string][]byte{lodiav1alpha1.KubeconfigKey: []byte("kubeconfig")}},
		"ca-source":         {Data: map[string][]byte{lodiav1alpha1.CACertKey: []byte("certificate")}},
		"kubeconfig-copy":   {Data: map[string][]byte{secret.KubeconfigDataName: []byte("kubeconfig")}},
		"ca-copy":           {Data: map[string][]byte{secret.TLSCrtDataName: []byte("certificate")}},
	}
	for id, s := range secrets {
		s.Namespace, s.Name, s.UID = upgradedKey.Namespace, upgradedNames[id], types.UID(id)
		if strings.HasSuffix(id, "-copy") {
			s.Type, s.Labels, s.OwnerReferences = clusterv1.ClusterSecretType, map[string]string{clusterv1.ClusterNameLabel: upgradedKey.Name}, owner
		}
	}
	return &upgraded{
		cluster: cluster, lcp: lcp, controlPlane: controlPlane, secrets: secrets,
		machines:   []*clusterv1.Machine{machineOfDeployment("m", "machine", "node", workersName(upgradedKey.Name))},
		deployment: managerDeployment("manager", releaseImage),
		binding:    &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: grantBinding, UID: "binding"}},
	}
}

func (u *upgraded) reader(t *testing.T) client.Client {
	t.Helper()
	objs := []client.Object{u.cluster, u.lcp, u.controlPlane, u.deployment}
	for _, s := range u.secrets {
		objs = append(objs, s)
	}
	for _, m := range u.machines {
		objs = append(objs, m)
	}
	if !u.bindingIsMissing {
		objs = append(objs, u.binding)
	}
	return readerOf(t, objs...)
}

func TestWhatTheReleaseMadeIsKeptWhenTheNewManagerAdoptsItAndChangesNothingTheClusterShows(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*upgraded)
		failure string
	}{
		{"every object is the one of before", func(*upgraded) {}, ""},
		{"a LodiaControlPlane that was made again", func(u *upgraded) { u.lcp.UID = "again" }, `v1alpha1.LodiaControlPlane w has the UID "again", before the upgrade "lcp"`},
		{"a ControlPlane that was made again", func(u *upgraded) { u.controlPlane.UID = "again" }, `v1alpha1.ControlPlane w has the UID "again"`},
		{"a kubeconfig that was made again", func(u *upgraded) { u.secrets["kubeconfig-copy"].UID = "again" }, `v1.Secret w-kubeconfig has the UID "again"`},
		{"a CA that was made again", func(u *upgraded) { u.secrets["ca-copy"].UID = "again" }, `v1.Secret w-ca has the UID "again"`},
		{"a kubeconfig that is not Lodia's", func(u *upgraded) { u.secrets["kubeconfig-copy"].Data[secret.KubeconfigDataName] = []byte("other") }, "they differ"},
		{"a copy that the LodiaControlPlane does not control", func(u *upgraded) { u.secrets["ca-copy"].OwnerReferences = nil }, "not controlled"},
		{"a Cluster whose Available moved", func(u *upgraded) { u.cluster.Status.Conditions[0].LastTransitionTime = madeAgainAt }, "the Available of the Cluster changed at"},
		{"a LodiaControlPlane whose Available moved", func(u *upgraded) { u.lcp.Status.Conditions[0].LastTransitionTime = madeAgainAt }, "the Available of the LodiaControlPlane changed at"},
		{"a Cluster that is not Available", func(u *upgraded) { u.cluster.Status.Conditions[0].Status = metav1.ConditionFalse }, "Cluster: Available is"},
		{"a LodiaControlPlane without Available", func(u *upgraded) { u.lcp.Status.Conditions = u.lcp.Status.Conditions[1:] }, "LodiaControlPlane: Available is nil"},
		{"a LodiaControlPlane whose Ready moved", func(u *upgraded) { u.lcp.Status.Conditions[1].LastTransitionTime = madeAgainAt }, ""},
		{"an Available that reads a copy of another object", func(u *upgraded) { u.lcp.Status.Conditions[0].Reason = v1alpha1.CopyOfAnotherObjectReason }, "the Available of LodiaControlPlane w is CopyOfAnotherObject"},
		{"a Ready that reads a copy of another object", func(u *upgraded) { u.lcp.Status.Conditions[1].Reason = v1alpha1.CopyOfAnotherObjectReason }, "the Ready of LodiaControlPlane w is CopyOfAnotherObject"},
		{"no copy", func(u *upgraded) { delete(u.secrets, "ca-copy") }, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUpgraded()
			before, err := readBeforeTheUpgrade(t.Context(), u.reader(t), upgradedKey)
			if err != nil {
				t.Fatal(err)
			}
			c.change(u)
			requireFailure(t, requireWhatTheReleaseMadeKept(t.Context(), u.reader(t), upgradedKey, before), c.failure)
		})
	}
}

func TestWhatIsReadBeforeTheUpgradeIsWhatTheClusterHolds(t *testing.T) {
	u := newUpgraded()
	got, err := readBeforeTheUpgrade(t.Context(), u.reader(t), upgradedKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.deployment != "manager" || got.binding != "binding" || got.worker.UID != "machine" || got.worker.Status.NodeRef.Name != "node" {
		t.Errorf("got %+v", got)
	}
	wantMade := map[string]types.UID{
		"*v1alpha1.LodiaControlPlane w": "lcp", "*v1alpha1.ControlPlane w": "control-plane", "*v1.Secret w-kubeconfig": "kubeconfig-copy", "*v1.Secret w-ca": "ca-copy",
	}
	if !maps.Equal(got.made, wantMade) {
		t.Errorf("the objects are %v, want %v", got.made, wantMade)
	}
	for what, since := range got.available {
		if !since.Equal(&madeAt) {
			t.Errorf("the Available of the %s is from %s, want %s", what, since, madeAt)
		}
	}
	if len(got.available) != 2 {
		t.Errorf("read the Available of %v, want the Cluster's and the LodiaControlPlane's", got.available)
	}
}

func TestNothingIsReadBeforeTheUpgradeFromAClusterThatIsNotAvailableWithOneWorker(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*upgraded)
		failure string
	}{
		{"a Cluster that is not Available", func(u *upgraded) { u.cluster.Status.Conditions = nil }, "Cluster: Available is"},
		{"a LodiaControlPlane that is not Available", func(u *upgraded) { u.lcp.Status.Conditions[0].Status = metav1.ConditionFalse }, "LodiaControlPlane: Available is"},
		{"no worker", func(u *upgraded) { u.machines = nil }, "0 Machines, want 1"},
		{"two workers", func(u *upgraded) { u.machines = append(u.machines, workerMachine("n", "other", "node-n")) }, "2 Machines, want 1"},
		{"a worker that does not run", func(u *upgraded) { u.machines[0].Status.Phase = string(clusterv1.MachinePhaseProvisioned) }, "is Provisioned"},
		{"no Deployment of the provider", func(u *upgraded) { u.deployment.Name = "other" }, "not found"},
		{"no binding of the grant", func(u *upgraded) { u.bindingIsMissing = true }, "not found"},
		{"no ControlPlane", func(u *upgraded) { u.controlPlane.Name = "other" }, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUpgraded()
			c.change(u)
			_, err := readBeforeTheUpgrade(t.Context(), u.reader(t), upgradedKey)
			requireFailure(t, err, c.failure)
		})
	}
}
