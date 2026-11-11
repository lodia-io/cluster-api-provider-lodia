// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"fmt"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestDeletionLeavesAForeignControlPlaneAlone(t *testing.T) {
	ns := namespace(t)
	previous, foreign := leftoverOfAnotherOwner(t, ns, "foreign")
	_, lcp := create(t, ns, "foreign")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)
	absent(t, lcp)
	get(t, foreign)
	if !foreign.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(foreign, previous) {
		t.Errorf("the foreign ControlPlane changed: deletion %v, owners %v", foreign.DeletionTimestamp, foreign.OwnerReferences)
	}
}

func leftoverOfAnotherOwner(t *testing.T, ns, name string) (*corev1.ConfigMap, *lodiav1alpha1.ControlPlane) {
	t.Helper()
	previous := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "previous"}}
	if err := k8s.Create(t.Context(), previous); err != nil {
		t.Fatal(err)
	}
	leftover := &lodiav1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(previous, corev1.SchemeGroupVersion.WithKind("ConfigMap"))},
		},
		Spec: lodiav1alpha1.ControlPlaneSpec{
			Version:   "v1.35.0",
			Datastore: lodiav1alpha1.DatastoreChoice{Shared: &lodiav1alpha1.SharedDatastore{Name: "shared"}},
			Endpoint:  lodiav1alpha1.Endpoint{LoadBalancer: &lodiav1alpha1.LoadBalancerEndpoint{}},
			Network:   lodiav1alpha1.Network{ServiceCIDRs: []lodiav1alpha1.CIDR{"10.96.0.0/12"}, PodCIDRs: []lodiav1alpha1.CIDR{"10.244.0.0/16"}},
		},
	}
	if err := k8s.Create(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}
	return previous, leftover
}

func TestALeftoverControlPlaneOfAnotherOwnerIsLeftAlone(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	previous, leftover := leftoverOfAnotherOwner(t, ns, "epsilon")
	cluster, lcp := create(t, ns, "epsilon")

	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.ObjectsControlledElsewhereReason,
			fmt.Sprintf("ControlPlane epsilon is controlled by ConfigMap previous (uid %s)", previous.UID))
	})
	cp := controlPlaneOf(cluster)
	get(t, cp)
	if cp.Spec.Version != "v1.35.0" || cp.Generation != leftover.Generation || !metav1.IsControlledBy(cp, previous) {
		t.Errorf("the leftover changed: version %s, generation %d, owners %v", cp.Spec.Version, cp.Generation, cp.OwnerReferences)
	}
}

func TestTheProviderTakesOverOnceALeftoverControlPlaneIsGone(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	_, leftover := leftoverOfAnotherOwner(t, ns, "epsilon-gone")
	cluster, lcp := create(t, ns, "epsilon-gone")
	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.ObjectsControlledElsewhereReason, "")
	})

	if err := k8s.Delete(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}
	cp := controlPlaneOf(cluster)
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); err != nil {
			return err
		}
		if !metav1.IsControlledBy(cp, lcp) {
			return fmt.Errorf("owners %v", cp.OwnerReferences)
		}
		return nil
	})
}

func TestALeftoverSecretOutsideTheCacheIsLeftAlone(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	leftover := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zeta-kubeconfig"},
		Data:       map[string][]byte{secret.KubeconfigDataName: []byte("a predecessor's kubeconfig")},
	}
	if err := k8s.Create(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}
	cluster, lcp := create(t, ns, "zeta")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	lodiaSecrets(t, cluster, ownedBy(cp))

	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.ObjectsControlledElsewhereReason, "Secret zeta-kubeconfig exists without a controller")
	})
	get(t, leftover)
	if string(leftover.Data[secret.KubeconfigDataName]) != "a predecessor's kubeconfig" || len(leftover.OwnerReferences) != 0 || len(leftover.Labels) != 0 {
		t.Errorf("the leftover changed: %v, owners %v, labels %v", leftover.Data, leftover.OwnerReferences, leftover.Labels)
	}
	get(t, secretOf(cluster, secret.ClusterCA))
}

func TestATargetHeldElsewhereDoesNotInitialize(t *testing.T) {
	cluster, lcp, cp, r := createdControlPlane(t, "held")
	setReady(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))
	held := secretOf(cluster, secret.ClusterCA)
	held.Type = clusterv1.ClusterSecretType
	held.Data = map[string][]byte{secret.TLSCrtDataName: []byte("another's")}
	if err := k8s.Create(t.Context(), held); err != nil {
		t.Fatal(err)
	}

	if err := pass(t.Context(), r, lcp); err == nil {
		t.Fatal("the pass returned no error")
	}
	get(t, lcp)
	if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
		t.Errorf("initialized with a target held elsewhere: %+v", lcp.Status)
	}
}

func TestAPredecessorsSecretsAreNotCopied(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "nu")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	predecessor := []metav1.OwnerReference{{
		APIVersion: lodiav1alpha1.GroupVersion.String(), Kind: "ControlPlane", Name: cluster.Name, UID: "the-predecessor", Controller: new(true),
	}}
	lodiaSecrets(t, cluster, predecessor)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)

	passes(t, r, lcp)
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
		t.Errorf("initialized from a predecessor's Secrets: %+v", lcp.Status)
	}

	for _, name := range []string{
		lodiav1alpha1.CABundleSecret.Name(cluster.Name), lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name), cluster.Name + "-cakey-cluster",
	} {
		if err := k8s.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	lodiaSecrets(t, cluster, ownedBy(cp))
	passes(t, r, lcp)
	get(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))
}

func TestATransientReadOfATargetSecretLeavesTheLeftoverAlone(t *testing.T) {
	cluster, lcp, cp, _ := createdControlPlane(t, "target-read")
	lodiaSecrets(t, cluster, ownedBy(cp))
	leftover := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: secret.Name(cluster.Name, secret.Kubeconfig)},
		Data:       map[string][]byte{secret.KubeconfigDataName: []byte("a predecessor's kubeconfig")},
	}
	if err := k8s.Create(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}

	unavailable := direct(t, interceptor.Funcs{Get: unavailableSecret(leftover.Name)})
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, leftover)
	if string(leftover.Data[secret.KubeconfigDataName]) != "a predecessor's kubeconfig" || len(leftover.OwnerReferences) != 0 {
		t.Errorf("the leftover changed: %v, owners %v", leftover.Data, leftover.OwnerReferences)
	}
}
