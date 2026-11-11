// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func TestATemplateKeepsEveryFieldItIsGiven(t *testing.T) {
	for name, mutate := range everyOptionalField {
		t.Run(name, func(t *testing.T) {
			template := &v1alpha1.LodiaControlPlaneTemplate{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace(t), Name: "full"},
				Spec: v1alpha1.LodiaControlPlaneTemplateSpec{Template: v1alpha1.LodiaControlPlaneTemplateResource{
					Spec: spec().LodiaControlPlaneTemplateResourceSpec,
				}},
			}
			mutate(&template.Spec.Template.Spec)
			want := template.Spec.Template.Spec.DeepCopy()
			if want.Layout == "" {
				want.Layout = lodiav1alpha1.LayoutSplit
			}

			if err := k8s.Create(t.Context(), template); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(*want, template.Spec.Template.Spec); diff != "" {
				t.Errorf("template spec (-sent +stored):\n%s", diff)
			}
		})
	}
}

func TestATemplateWithCompactAndHighAvailabilityIsRefused(t *testing.T) {
	template := &v1alpha1.LodiaControlPlaneTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace(t), Name: "compact-ha"},
		Spec: v1alpha1.LodiaControlPlaneTemplateSpec{Template: v1alpha1.LodiaControlPlaneTemplateResource{
			Spec: spec().LodiaControlPlaneTemplateResourceSpec,
		}},
	}
	template.Spec.Template.Spec.Layout = lodiav1alpha1.LayoutCompact
	template.Spec.Template.Spec.HighAvailability = &lodiav1alpha1.HighAvailability{FailureTolerance: "Node"}

	err := k8s.Create(t.Context(), template)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "the Compact layout runs one replica of everything: remove highAvailability") {
		t.Errorf("creating a Compact template with highAvailability returned %v", err)
	}
}
