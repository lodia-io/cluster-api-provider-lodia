// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestAClusterGetsItsControlPlaneAndTheContract(t *testing.T) {
	startManager(t)
	cluster, lcp := create(t, namespace(t), "alpha")

	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	if !metav1.IsControlledBy(cp, lcp) || cp.Labels[clusterv1.ClusterNameLabel] != "alpha" {
		t.Errorf("ControlPlane owners %v, labels %v", cp.OwnerReferences, cp.Labels)
	}
	want := lodiav1alpha1.ControlPlaneSpec{
		Version:     "v1.35.8",
		Layout:      lodiav1alpha1.LayoutSplit,
		Datastore:   lcp.Spec.Datastore,
		Endpoint:    lcp.Spec.Endpoint,
		Placement:   lcp.Spec.Placement,
		Maintenance: lcp.Spec.Maintenance,
		Health:      lcp.Spec.Health,
		Network: lodiav1alpha1.Network{
			ServiceCIDRs:  []lodiav1alpha1.CIDR{"10.128.0.0/12"},
			PodCIDRs:      []lodiav1alpha1.CIDR{"192.168.0.0/16"},
			ClusterDomain: "tenant.local",
		},
	}
	if diff := cmp.Diff(want, cp.Spec); diff != "" {
		t.Errorf("ControlPlane spec (-want +got):\n%s", diff)
	}

	setReady(t, cp)
	issued := lodiaSecrets(t, cluster, ownedBy(cp))

	eventually(t, func() error {
		get(t, lcp)
		switch {
		case lcp.Spec.ControlPlaneEndpoint != clusterv1.APIEndpoint{Host: "203.0.113.10", Port: 31443}:
			return fmt.Errorf("endpoint %+v", lcp.Spec.ControlPlaneEndpoint)
		case lcp.Status.Initialization.ControlPlaneInitialized == nil || !*lcp.Status.Initialization.ControlPlaneInitialized:
			return errors.New("not initialized")
		case !meta.IsStatusConditionTrue(lcp.Status.Conditions, clusterv1.AvailableCondition):
			return fmt.Errorf("conditions %+v", lcp.Status.Conditions)
		case lcp.Status.Version != "v1.35.8" || len(lcp.Status.Versions) != 1 || lcp.Status.Versions[0].Version != "v1.35.8":
			return fmt.Errorf("version %q, versions %+v", lcp.Status.Version, lcp.Status.Versions)
		}
		return ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, "")
	})
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.AvailableCondition); c.Reason != lodiav1alpha1.ReasonReachable {
		t.Errorf("Available %+v, want Reachable's reason", c)
	}
	if lcp.Status.ExternalManagedControlPlane == nil || !*lcp.Status.ExternalManagedControlPlane {
		t.Error("externalManagedControlPlane is not true")
	}
	if _, ok := lcp.Annotations[clusterctlv1.BlockMoveAnnotation]; !ok {
		t.Errorf("annotations %v lack %s", lcp.Annotations, clusterctlv1.BlockMoveAnnotation)
	}

	admin := &corev1.Secret{}
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}, admin); err != nil {
		t.Fatal(err)
	}
	for purpose, data := range map[secret.Purpose]map[string][]byte{
		secret.Kubeconfig: {secret.KubeconfigDataName: admin.Data[lodiav1alpha1.KubeconfigKey]},
		secret.ClusterCA:  {secret.TLSCrtDataName: issued.cert},
	} {
		s := secretOf(cluster, purpose)
		eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(s), s) })
		if s.Type != clusterv1.ClusterSecretType || s.Labels[clusterv1.ClusterNameLabel] != "alpha" || !metav1.IsControlledBy(s, lcp) {
			t.Errorf("%s: type %s, labels %v, owners %v", s.Name, s.Type, s.Labels, s.OwnerReferences)
		}
		if diff := cmp.Diff(data, s.Data); diff != "" {
			t.Errorf("%s data (-want +got):\n%s", s.Name, diff)
		}
	}

	written := &corev1.SecretList{}
	if err := k8s.List(t.Context(), written, client.InNamespace(cluster.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, s := range written.Items {
		if !metav1.IsControlledBy(&s, lcp) {
			continue
		}
		for key, value := range s.Data {
			if key == secret.TLSKeyDataName || bytes.Contains(value, []byte("PRIVATE KEY")) || bytes.Contains(value, issued.key) {
				t.Errorf("Secret %s holds a private key under %s", s.Name, key)
			}
		}
	}
}

func TestEveryOptionalFieldReachesTheControlPlane(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "full", func(_ *clusterv1.Cluster, l *v1alpha1.LodiaControlPlane) {
		l.Spec.Layout = lodiav1alpha1.LayoutSplit
		l.Spec.HighAvailability = &lodiav1alpha1.HighAvailability{FailureTolerance: "Node"}
		l.Spec.APIServer = &lodiav1alpha1.APIServer{Replicas: new(int32(2))}
		l.Spec.ControllerManager = &lodiav1alpha1.Component{ExtraArgs: map[string]string{"concurrent-deployment-syncs": "3"}}
		l.Spec.Scheduler = &lodiav1alpha1.Scheduler{ExtraArgs: map[string]string{"kube-api-qps": "100"}}
		l.Spec.Addons = &lodiav1alpha1.Addons{CoreDNS: &lodiav1alpha1.CoreDNS{Replicas: new(int32(3))}}
		l.Spec.ServiceAccount = &lodiav1alpha1.ServiceAccount{Issuer: "https://issuer.example.com"}
		l.Spec.RestoreFrom = &lodiav1alpha1.RestoreFrom{BackupRef: lodiav1alpha1.LocalReference{Name: "backup"}}
	})
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	get(t, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	want := lodiav1alpha1.ControlPlaneSpec{
		Version:           lcp.Spec.Version,
		Layout:            lcp.Spec.Layout,
		HighAvailability:  lcp.Spec.HighAvailability,
		Datastore:         lcp.Spec.Datastore,
		Endpoint:          lcp.Spec.Endpoint,
		APIServer:         lcp.Spec.APIServer,
		ControllerManager: lcp.Spec.ControllerManager,
		Scheduler:         lcp.Spec.Scheduler,
		Addons:            lcp.Spec.Addons,
		Placement:         lcp.Spec.Placement,
		Maintenance:       lcp.Spec.Maintenance,
		Health:            lcp.Spec.Health,
		ServiceAccount:    lcp.Spec.ServiceAccount,
		RestoreFrom:       lcp.Spec.RestoreFrom,
		Network: lodiav1alpha1.Network{
			ServiceCIDRs:  []lodiav1alpha1.CIDR{"10.128.0.0/12"},
			PodCIDRs:      []lodiav1alpha1.CIDR{"192.168.0.0/16"},
			ClusterDomain: "tenant.local",
		},
	}
	if diff := cmp.Diff(want, cp.Spec); diff != "" {
		t.Errorf("ControlPlane spec (-want +got):\n%s", diff)
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.Spec.Placement = nil; l.Spec.ServiceAccount = nil })
	passes(t, r, lcp)
	get(t, cp)
	if cp.Spec.Placement != nil || cp.Spec.ServiceAccount != nil {
		t.Errorf("removed fields stayed: placement %+v, service account %+v", cp.Spec.Placement, cp.Spec.ServiceAccount)
	}
}

func TestAnEditOfAFieldTheProviderOwnsIsReverted(t *testing.T) {
	_, lcp, cp, r, _ := readyControlPlane(t, "edited")
	before := cp.DeepCopy()
	cp.Spec.Version = "v1.35.9"
	if err := k8s.Patch(t.Context(), cp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, cp)
	if cp.Spec.Version != "v1.35.8" {
		t.Errorf("version %s, want it reverted", cp.Spec.Version)
	}
}

func TestCABPKAcceptsTheCASecretWithoutAKey(t *testing.T) {
	cluster, _, _, _, issued := readyControlPlane(t, "rho")
	copied := secretOf(cluster, secret.ClusterCA)
	get(t, copied)
	if _, ok := copied.Data[secret.TLSKeyDataName]; ok {
		t.Fatalf("%s holds %s", copied.Name, secret.TLSKeyDataName)
	}

	certificates := secret.NewCertificatesForWorker("")
	if err := certificates.Lookup(t.Context(), k8s, client.ObjectKeyFromObject(cluster)); err != nil {
		t.Fatal(err)
	}
	if err := certificates.EnsureAllExist(); err != nil {
		t.Fatal(err)
	}
	ca := certificates.GetByPurpose(secret.ClusterCA)
	hashes, err := ca.Hashes()
	if err != nil || len(hashes) != 1 || !bytes.Equal(ca.KeyPair.Cert, issued.cert) {
		t.Errorf("hashes %v, %v; certificate %s", hashes, err, ca.KeyPair.Cert)
	}
}
