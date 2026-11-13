// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

func TestAFailureOnOneSecretDoesNotStopTheOther(t *testing.T) {
	cluster, lcp, cp, _ := createdControlPlane(t, "chi")
	lodiaSecrets(t, cluster, ownedBy(cp))

	kubeconfigErr, caErr := errors.New("the kubeconfig write failed"), errors.New("the CA write failed")
	failing := map[string]error{"chi-kubeconfig": kubeconfigErr}
	broken := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if s, ok := obj.(*corev1ac.SecretApplyConfiguration); ok && failing[*s.Name] != nil {
				return failing[*s.Name]
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), broken, lcp); !errors.Is(err, kubeconfigErr) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))

	failing["chi-ca"] = caErr
	err := pass(t.Context(), broken, lcp)
	if !errors.Is(err, kubeconfigErr) || !errors.Is(err, caErr) {
		t.Fatalf("the pass returned %v, want both errors", err)
	}
}

func TestASourceWithoutItsKeyDoesNotInitialize(t *testing.T) {
	cluster, lcp, cp, r := createdControlPlane(t, "keyless")
	setReady(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))
	admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}}
	get(t, admin)
	admin.Data = map[string][]byte{"other": []byte("x")}
	if err := k8s.Update(t.Context(), admin); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)
	get(t, lcp)
	if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
		t.Errorf("initialized with a source without its key: %+v", lcp.Status)
	}
}

func TestASourceWithoutItsKeyKeepsTheLastGoodCopy(t *testing.T) {
	cluster, lcp, _, r, issued := readyControlPlane(t, "sigma")
	admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}}
	get(t, admin)
	kubeconfig := admin.Data[lodiav1alpha1.KubeconfigKey]
	admin.Data = map[string][]byte{"other": []byte("x")}
	if err := k8s.Update(t.Context(), admin); err != nil {
		t.Fatal(err)
	}
	bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.CABundleSecret.Name(cluster.Name)}}
	get(t, bundle)
	bundle.Data[lodiav1alpha1.CACertKey] = []byte{}
	if err := k8s.Update(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)

	copied := secretOf(cluster, secret.Kubeconfig)
	get(t, copied)
	if !bytes.Equal(copied.Data[secret.KubeconfigDataName], kubeconfig) {
		t.Errorf("the good kubeconfig was replaced by %q", copied.Data[secret.KubeconfigDataName])
	}
	ca := secretOf(cluster, secret.ClusterCA)
	get(t, ca)
	if !bytes.Equal(ca.Data[secret.TLSCrtDataName], issued.cert) {
		t.Errorf("the good certificate was replaced by %q", ca.Data[secret.TLSCrtDataName])
	}
}

func TestASourceWithoutItsKeyWritesNoCopy(t *testing.T) {
	cluster, lcp, cp, r := createdControlPlane(t, "upsilon")
	for name, data := range map[string]map[string][]byte{
		lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name): {"wrong": []byte("x")},
		lodiav1alpha1.CABundleSecret.Name(cluster.Name):        {lodiav1alpha1.CACertKey: {}},
	} {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name, Labels: map[string]string{lodiav1alpha1.ControlPlaneLabel: cluster.Name}, OwnerReferences: ownedBy(cp)},
			Data:       data,
		}
		if err := k8s.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}

	passes(t, r, lcp)
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
}

func unavailableSecret(name string) func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok && key.Name == name {
			return apierrors.NewServiceUnavailable("the API server is restarting")
		}
		return c.Get(ctx, key, obj, opts...)
	}
}

func TestATransientReadOfASourceSecretIsReturned(t *testing.T) {
	cluster, lcp, _, _, _ := readyControlPlane(t, "source-read")
	copied := secretOf(cluster, secret.Kubeconfig)
	get(t, copied)
	kubeconfig := copied.Data[secret.KubeconfigDataName]
	bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.CABundleSecret.Name(cluster.Name)}}
	get(t, bundle)
	bundle.Data[lodiav1alpha1.CACertKey] = []byte("a renewed CA")
	if err := k8s.Update(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}

	unavailable := direct(t, interceptor.Funcs{Get: unavailableSecret(lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name))})
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, copied)
	if !bytes.Equal(copied.Data[secret.KubeconfigDataName], kubeconfig) {
		t.Errorf("the copy changed to %q", copied.Data[secret.KubeconfigDataName])
	}
	ca := secretOf(cluster, secret.ClusterCA)
	get(t, ca)
	if string(ca.Data[secret.TLSCrtDataName]) != "a renewed CA" {
		t.Errorf("the other copy did not follow its source: %q", ca.Data[secret.TLSCrtDataName])
	}
}

func TestAResyncRepairsTheProvidersSecrets(t *testing.T) {
	ns := namespace(t)
	startManagerWith(t, controller.Config{Namespace: ns, Concurrency: 10, SyncPeriod: time.Second})
	cluster, _ := create(t, ns, "pi")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	issued := lodiaSecrets(t, cluster, ownedBy(cp))
	copied := secretOf(cluster, secret.ClusterCA)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(copied), copied) })

	copied.Data[secret.TLSCrtDataName] = []byte("edited by hand")
	if err := k8s.Update(t.Context(), copied); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(copied), copied); err != nil {
			return err
		}
		if !bytes.Equal(copied.Data[secret.TLSCrtDataName], issued.cert) {
			return fmt.Errorf("tls.crt is %s", copied.Data[secret.TLSCrtDataName])
		}
		return nil
	})
}

func TestARenewedSourceReachesItsCopyWithoutAResync(t *testing.T) {
	ns := namespace(t)
	startManager(t, ns)
	cluster, lcp := create(t, ns, "renew")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	setReady(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))
	eventually(t, func() error {
		get(t, lcp)
		if i := lcp.Status.Initialization.ControlPlaneInitialized; i == nil || !*i {
			return errors.New("not initialized")
		}
		return nil
	})
	settled(t, lcp)

	for _, c := range []struct {
		source, key string
		copy        secret.Purpose
		copyKey     string
	}{
		{lodiav1alpha1.CABundleSecret.Name(cluster.Name), lodiav1alpha1.CACertKey, secret.ClusterCA, secret.TLSCrtDataName},
		{lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name), lodiav1alpha1.KubeconfigKey, secret.Kubeconfig, secret.KubeconfigDataName},
	} {
		renewed := []byte("renewed " + c.source)
		source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: c.source}}
		get(t, source)
		source.Data[c.key] = renewed
		if err := k8s.Update(t.Context(), source); err != nil {
			t.Fatal(err)
		}
		copied := secretOf(cluster, c.copy)
		eventually(t, func() error {
			get(t, copied)
			if !bytes.Equal(copied.Data[c.copyKey], renewed) {
				return fmt.Errorf("%s still holds %.40q", copied.Name, copied.Data[c.copyKey])
			}
			return nil
		})
	}
}

func TestTheCacheHoldsOnlyLodiasSecretsAndOnlyTheirMetadata(t *testing.T) {
	ns := namespace(t)
	mgr := startManager(t, ns)
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other"}}
	lodias := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "lodias", Labels: map[string]string{lodiav1alpha1.ControlPlaneLabel: "x"}}}
	for _, s := range []*corev1.Secret{other, lodias} {
		if err := k8s.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	metadata := func() *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
	}

	eventually(t, func() error { return mgr.GetCache().Get(t.Context(), client.ObjectKeyFromObject(lodias), metadata()) })
	if err := mgr.GetCache().Get(t.Context(), client.ObjectKeyFromObject(other), metadata()); !apierrors.IsNotFound(err) {
		t.Errorf("a Secret without Lodia's label is in the cache: %v", err)
	}
	var notCached *cache.ErrResourceNotCached
	if err := mgr.GetCache().Get(t.Context(), client.ObjectKeyFromObject(lodias), &corev1.Secret{}); !errors.As(err, &notCached) {
		t.Errorf("reading a Secret's data through the cache returned %v", err)
	}
}
