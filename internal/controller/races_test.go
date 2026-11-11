// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"context"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestAnInitializedControlPlaneIsNotRecreated(t *testing.T) {
	cluster, lcp, cp, r, issued := readyControlPlane(t, "xi")
	if err := k8s.Delete(t.Context(), cp); err != nil {
		t.Fatal(err)
	}

	stale := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if l, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				l.Status.Initialization.ControlPlaneInitialized = nil
			}
			return err
		},
	})
	stale.APIReader = r.APIReader
	passes(t, stale, lcp)
	passes(t, r, lcp)

	absent(t, cp)
	get(t, lcp)
	initialized(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason, "ControlPlane xi is gone"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason)
	kept := secretOf(cluster, secret.ClusterCA)
	get(t, kept)
	if !bytes.Equal(kept.Data[secret.TLSCrtDataName], issued.cert) {
		t.Error("the CA changed")
	}
}

func TestAControlPlaneVanishingBeforeTheApplyIsNotRecreated(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "tau")
	racing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsConflict(err) {
		t.Fatalf("the pass returned %v, want a conflict on the UID", err)
	}
	absent(t, cp)

	passes(t, r, lcp)
	absent(t, cp)
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason, "ControlPlane tau is gone"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason)
}

func TestAControlPlaneReplacedBeforeTheApplyIsLeftAlone(t *testing.T) {
	cluster, lcp, cp, _, _ := readyControlPlane(t, "tau-replaced")
	replacement := cp.DeepCopy()
	replacement.ObjectMeta = metav1.ObjectMeta{Namespace: cp.Namespace, Name: cp.Name, Finalizers: []string{lodiav1alpha1.DatastoreFinalizer}}
	racing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok && replacement.UID == "" {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
				if err := k8s.Create(ctx, replacement); err != nil {
					return err
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsInvalid(err) {
		t.Fatalf("the pass returned %v, want the UID refused", err)
	}
	got := controlPlaneOf(cluster)
	get(t, got)
	if got.UID != replacement.UID || got.Generation != replacement.Generation || metav1.GetControllerOf(got) != nil {
		t.Errorf("the replacement changed: uid %s, generation %d, owners %v", got.UID, got.Generation, got.OwnerReferences)
	}
}
