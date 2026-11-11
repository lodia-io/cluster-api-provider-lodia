// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestANameLodiaRefusesIsReportedAndNothingIsCreated(t *testing.T) {
	startManager(t)
	name := strings.Repeat("a", 33)
	cluster, lcp := create(t, namespace(t), name)
	lodiaSecrets(t, cluster, nil)

	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.RefusedReason,
			fmt.Sprintf(`ControlPlane.lodia.io %q is invalid: <nil>: Invalid value: the name must be a DNS-1035 label of at most 32 characters`, name))
	})
	absent(t, controlPlaneOf(cluster), secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
}

func TestInitializationWaitsForBothSecrets(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "omega")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)
	notInitialized := func() {
		t.Helper()
		passes(t, r, lcp)
		get(t, lcp)
		if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
			t.Errorf("initialized without both Secrets: %+v", lcp.Status)
		}
		if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
			t.Error(err)
		}
	}
	notInitialized()

	issued := lodiaSecrets(t, cluster, ownedBy(cp))
	bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.CABundleSecret.Name(cluster.Name)}}
	if err := k8s.Delete(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	notInitialized()
	get(t, secretOf(cluster, secret.Kubeconfig))

	restored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: bundle.Name, Labels: map[string]string{lodiav1alpha1.ControlPlaneLabel: cluster.Name}, OwnerReferences: ownedBy(cp)},
		Data:       map[string][]byte{lodiav1alpha1.CACertKey: issued.cert},
	}
	if err := k8s.Create(t.Context(), restored); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)

	if err := k8s.Delete(t.Context(), restored); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)
}

func TestReadyAtAnOlderGenerationInitializes(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "older-generation")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.Spec.Version = "v1.35.9" })
	passes(t, r, lcp)
	get(t, cp)
	if c := meta.FindStatusCondition(cp.Status.Conditions, lodiav1alpha1.ConditionReady); c == nil || c.ObservedGeneration == cp.Generation {
		t.Fatalf("Ready %+v at generation %d", c, cp.Generation)
	}

	lodiaSecrets(t, cluster, ownedBy(cp))
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)
}

func TestAForbiddenWriteIsReportedAsRefused(t *testing.T) {
	isControlPlane := func(obj runtime.ApplyConfiguration) bool {
		_, isSecret := obj.(*corev1ac.SecretApplyConfiguration)
		return !isSecret
	}
	for name, target := range map[string]func(obj runtime.ApplyConfiguration) bool{
		"the ControlPlane": isControlPlane,
		"a Secret":         func(obj runtime.ApplyConfiguration) bool { return isSecretApply(obj, "psi-ca") },
	} {
		t.Run(name, func(t *testing.T) {
			_, lcp, _, _, _ := readyControlPlane(t, "psi")
			forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "objects"}, "x", errors.New("the namespace is terminating"))
			r := direct(t, interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					if target(obj) {
						return forbidden
					}
					return c.Apply(ctx, obj, opts...)
				},
			})
			if err := pass(t.Context(), r, lcp); !apierrors.IsForbidden(err) {
				t.Fatalf("the pass returned %v", err)
			}
			get(t, lcp)
			if err := ready(lcp, metav1.ConditionFalse, v1alpha1.RefusedReason, "the namespace is terminating"); err != nil {
				t.Error(err)
			}
			available(t, lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReachable)
		})
	}
}

func TestAControlPlaneWithNoStatusIsNotYetReported(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "no-status")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	get(t, controlPlaneOf(cluster))

	get(t, lcp)
	if err := ready(lcp, metav1.ConditionUnknown, conditions.NotYetReportedReason, ""); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionUnknown, conditions.NotYetReportedReason)
}

func TestReadyIsMirroredOnlyAtTheCurrentGeneration(t *testing.T) {
	_, lcp, cp, r, _ := readyControlPlane(t, "lambda")
	if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
		t.Fatal(err)
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.Spec.Version = "v1.35.9" })
	passes(t, r, lcp)
	get(t, cp)
	if cp.Generation != 2 || cp.Spec.Version != "v1.35.9" {
		t.Fatalf("generation %d, version %s", cp.Generation, cp.Spec.Version)
	}
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionUnknown, lodiav1alpha1.ReasonLatestGenerationNotObserved, "generation 1 of 2"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReachable)
	initialized(t, lcp)

	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type: lodiav1alpha1.ConditionReachable, Status: metav1.ConditionFalse, Reason: lodiav1alpha1.ReasonExternalProbeFailed,
		Message: "the public endpoint does not answer", ObservedGeneration: 1,
	})
	if err := k8s.Status().Update(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, lcp)
	available(t, lcp, metav1.ConditionFalse, lodiav1alpha1.ReasonExternalProbeFailed)

	setReady(t, cp)
	passes(t, r, lcp)
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
		t.Error(err)
	}
}

func TestARefusedChangeStillCopiesAndReports(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "mu")

	before := cluster.DeepCopy()
	cluster.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.200.0.0/16"}
	if err := k8s.Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	get(t, cp)
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type: lodiav1alpha1.ConditionReachable, Status: metav1.ConditionFalse, Reason: lodiav1alpha1.ReasonExternalProbeFailed,
		Message: "the public endpoint does not answer", ObservedGeneration: cp.Generation,
	})
	if err := k8s.Status().Update(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	renewed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}}
	get(t, renewed)
	renewed.Data[lodiav1alpha1.KubeconfigKey] = []byte("a renewed kubeconfig")
	if err := k8s.Update(t.Context(), renewed); err != nil {
		t.Fatal(err)
	}

	if err := pass(t.Context(), r, lcp); !apierrors.IsInvalid(err) {
		t.Fatalf("the pass returned %v, want the refusal", err)
	}
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.RefusedReason, "network is immutable"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, lodiav1alpha1.ReasonExternalProbeFailed)
	copied := secretOf(cluster, secret.Kubeconfig)
	get(t, copied)
	if string(copied.Data[secret.KubeconfigDataName]) != "a renewed kubeconfig" {
		t.Errorf("the kubeconfig did not follow the renewal: %s", copied.Data[secret.KubeconfigDataName])
	}
}
