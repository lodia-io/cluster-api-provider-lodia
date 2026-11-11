// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func requireCopies(ctx context.Context, k8s client.Reader, key client.ObjectKey, lcp *v1alpha1.LodiaControlPlane) error {
	for _, c := range []struct {
		from    lodiav1alpha1.Secret
		fromKey string
		purpose secret.Purpose
		toKey   string
	}{
		{lodiav1alpha1.AdminKubeconfigSecret, lodiav1alpha1.KubeconfigKey, secret.Kubeconfig, secret.KubeconfigDataName},
		{lodiav1alpha1.CABundleSecret, lodiav1alpha1.CACertKey, secret.ClusterCA, secret.TLSCrtDataName},
	} {
		source, copied := &corev1.Secret{}, &corev1.Secret{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: c.from.Name(key.Name)}, source); err != nil {
			return err
		}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: secret.Name(key.Name, c.purpose)}, copied); err != nil {
			return err
		}
		want := source.Data[c.fromKey]
		if len(want) == 0 {
			return fmt.Errorf("Lodia's Secret %s holds no %s", source.Name, c.fromKey)
		}
		if got := copied.Data[c.toKey]; !bytes.Equal(got, want) {
			return fmt.Errorf("Secret %s holds %d bytes under %s, Lodia's %s holds %d under %s, and they differ", copied.Name, len(got), c.toKey, source.Name, len(want), c.fromKey)
		}
		if len(copied.Data) != 1 {
			return fmt.Errorf("Secret %s holds the keys %v, want %s alone: the CA's key never leaves Lodia", copied.Name, slices.Sorted(maps.Keys(copied.Data)), c.toKey)
		}
		if copied.Type != clusterv1.ClusterSecretType {
			return fmt.Errorf("Secret %s has type %q, want %q", copied.Name, copied.Type, clusterv1.ClusterSecretType)
		}
		if got := copied.Labels[clusterv1.ClusterNameLabel]; got != key.Name {
			return fmt.Errorf("Secret %s has the label %s=%q, want %q", copied.Name, clusterv1.ClusterNameLabel, got, key.Name)
		}
		if !metav1.IsControlledBy(copied, lcp) {
			return fmt.Errorf("Secret %s is not controlled by LodiaControlPlane %s", copied.Name, lcp.Name)
		}
	}
	return nil
}

func TestTheCopiesAreLodiasSecretsWithTheContractsTypeAndLabel(t *testing.T) {
	key := client.ObjectKey{Namespace: "capi-join", Name: "c"}
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: types.UID("lcp")}}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(lcp, v1alpha1.GroupVersion.WithKind("LodiaControlPlane"))}
	secrets := func(change func(map[string]*corev1.Secret)) client.Reader {
		all := map[string]*corev1.Secret{
			"kubeconfig-source": {Data: map[string][]byte{lodiav1alpha1.KubeconfigKey: []byte("kubeconfig")}},
			"ca-source":         {Data: map[string][]byte{lodiav1alpha1.CACertKey: []byte("certificate")}},
			"kubeconfig-copy":   {Data: map[string][]byte{secret.KubeconfigDataName: []byte("kubeconfig")}},
			"ca-copy":           {Data: map[string][]byte{secret.TLSCrtDataName: []byte("certificate")}},
		}
		names := map[string]string{
			"kubeconfig-source": lodiav1alpha1.AdminKubeconfigSecret.Name(key.Name), "ca-source": lodiav1alpha1.CABundleSecret.Name(key.Name),
			"kubeconfig-copy": secret.Name(key.Name, secret.Kubeconfig), "ca-copy": secret.Name(key.Name, secret.ClusterCA),
		}
		for id, s := range all {
			s.Namespace, s.Name = key.Namespace, names[id]
			if strings.HasSuffix(id, "-copy") {
				s.Type, s.Labels, s.OwnerReferences = clusterv1.ClusterSecretType, map[string]string{clusterv1.ClusterNameLabel: key.Name}, owner
			}
		}
		change(all)
		builder := fake.NewClientBuilder()
		for _, s := range all {
			builder = builder.WithObjects(s)
		}
		return builder.Build()
	}
	cases := []struct {
		name, want string
		change     func(map[string]*corev1.Secret)
	}{
		{"copies of Lodia's Secrets", "", func(map[string]*corev1.Secret) {}},
		{"a kubeconfig of other bytes", "they differ", func(s map[string]*corev1.Secret) {
			s["kubeconfig-copy"].Data[secret.KubeconfigDataName] = []byte("other")
		}},
		{"a certificate of other bytes", "they differ", func(s map[string]*corev1.Secret) { s["ca-copy"].Data[secret.TLSCrtDataName] = []byte("other") }},
		{"a source with nothing in it", "holds no", func(s map[string]*corev1.Secret) {
			s["ca-source"].Data[lodiav1alpha1.CACertKey] = nil
			s["ca-copy"].Data[secret.TLSCrtDataName] = nil
		}},
		{"the CA's key beside the certificate", "alone", func(s map[string]*corev1.Secret) { s["ca-copy"].Data[secret.TLSKeyDataName] = []byte("key") }},
		{"the wrong type", "has type", func(s map[string]*corev1.Secret) { s["kubeconfig-copy"].Type = corev1.SecretTypeOpaque }},
		{"no cluster label", "has the label", func(s map[string]*corev1.Secret) { s["ca-copy"].Labels = nil }},
		{"another cluster's label", "has the label", func(s map[string]*corev1.Secret) { s["ca-copy"].Labels[clusterv1.ClusterNameLabel] = "other" }},
		{"no owner", "not controlled", func(s map[string]*corev1.Secret) { s["kubeconfig-copy"].OwnerReferences = nil }},
		{"an owner reference that is not the controller", "not controlled", func(s map[string]*corev1.Secret) {
			ref := owner[0]
			ref.Controller = new(false)
			s["kubeconfig-copy"].OwnerReferences = []metav1.OwnerReference{ref}
		}},
		{"another LodiaControlPlane's controller", "not controlled", func(s map[string]*corev1.Secret) {
			ref := owner[0]
			ref.UID = types.UID("other")
			s["ca-copy"].OwnerReferences = []metav1.OwnerReference{ref}
		}},
		{"no copy", "not found", func(s map[string]*corev1.Secret) { delete(s, "ca-copy") }},
		{"no source", "not found", func(s map[string]*corev1.Secret) { delete(s, "kubeconfig-source") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireCopies(t.Context(), secrets(c.change), key, lcp)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
