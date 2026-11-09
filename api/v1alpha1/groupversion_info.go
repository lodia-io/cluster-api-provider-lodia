// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 is the API of Lodia's Cluster API control plane provider.
// +kubebuilder:object:generate=true
// +groupName=controlplane.lodia.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is controlplane.lodia.io/v1alpha1.
	GroupVersion = schema.GroupVersion{Group: "controlplane.lodia.io", Version: "v1alpha1"}

	// SchemeBuilder registers the kinds of this version.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the kinds of this version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&LodiaControlPlane{}, &LodiaControlPlaneList{},
		&LodiaControlPlaneTemplate{}, &LodiaControlPlaneTemplateList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
