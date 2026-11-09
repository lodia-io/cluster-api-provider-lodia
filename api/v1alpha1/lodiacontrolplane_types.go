// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Finalizer holds a LodiaControlPlane until its ControlPlane is gone.
const Finalizer = "controlplane.lodia.io/lodiacontrolplane"

// The reasons of Ready that are the provider's own; otherwise Ready mirrors
// the ControlPlane's.
const (
	// RefusedReason: the API server refuses the ControlPlane or a Secret the
	// provider writes; the message is the server's.
	RefusedReason = "Refused"

	// ObjectsControlledElsewhereReason: the ControlPlane or a Secret the
	// provider writes exists with another controller or none, named in the
	// message; the provider leaves it alone.
	ObjectsControlledElsewhereReason = "ObjectsControlledElsewhere"

	// ControlPlaneNotFoundReason: the ControlPlane of an initialized
	// LodiaControlPlane is gone; the provider does not recreate it.
	ControlPlaneNotFoundReason = "ControlPlaneNotFound"
)

// LodiaControlPlane is a Cluster API control plane served by one Lodia
// ControlPlane, named after the Cluster and in its namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=lodiacontrolplanes,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io="
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Initialized",type=boolean,JSONPath=".status.initialization.controlPlaneInitialized"
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=".status.conditions[?(@.type=='Available')].status"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type LodiaControlPlane struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec LodiaControlPlaneSpec `json:"spec"`

	// +optional
	Status LodiaControlPlaneStatus `json:"status,omitzero"`
}

// LodiaControlPlaneList is a list of LodiaControlPlanes.
// +kubebuilder:object:root=true
type LodiaControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LodiaControlPlane `json:"items"`
}

// LodiaControlPlaneSpec is the ControlPlane's spec but its network, which
// comes from the Cluster, and its pause, which stays the ControlPlane's.
// +kubebuilder:validation:XValidation:rule="has(self.restoreFrom) == has(oldSelf.restoreFrom)",message="restoreFrom can be set only when the ControlPlane is created, and never removed"
type LodiaControlPlaneSpec struct {
	// Version is the Kubernetes version, vMAJOR.MINOR.PATCH. The minor never
	// decreases; Lodia upgrades one minor at a time towards it.
	// +required
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^v1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
	// +kubebuilder:validation:XValidation:rule="int(self.split('.')[1]) >= int(oldSelf.split('.')[1])",message="the Kubernetes minor version cannot decrease"
	Version string `json:"version"`

	// ControlPlaneEndpoint is the API server's public endpoint, which the
	// provider copies from the ControlPlane's status.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`

	LodiaControlPlaneTemplateResourceSpec `json:",inline"`

	// RestoreFrom starts the datastore from a Backup; set only at creation.
	// +optional
	RestoreFrom *lodiav1alpha1.RestoreFrom `json:"restoreFrom,omitempty"`
}

// LodiaControlPlaneStatus is what the provider reports to Cluster API.
type LodiaControlPlaneStatus struct {
	// Conditions are Available, the ControlPlane's Reachable; Ready, the
	// ControlPlane's Ready at its current generation unless the provider cannot
	// write its objects; and Paused.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Initialization is set once the ControlPlane is Ready and both Secrets are
	// copied.
	// +optional
	Initialization LodiaControlPlaneInitializationStatus `json:"initialization,omitempty,omitzero"`

	// ExternalManagedControlPlane is true: the control plane has no Machines
	// and no Nodes.
	// +optional
	ExternalManagedControlPlane *bool `json:"externalManagedControlPlane,omitempty"`

	// Version is the oldest Kubernetes version any part of the control plane
	// runs, the ControlPlane's status.kubernetes.version.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16
	Version string `json:"version,omitempty"`

	// Versions holds one entry, Version, as the contract prefers.
	// +optional
	// +listType=map
	// +listMapKey=version
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	Versions []clusterv1.StatusVersion `json:"versions,omitempty"`
}

// LodiaControlPlaneInitializationStatus is the initialization the Cluster
// API contract reads.
// +kubebuilder:validation:MinProperties=1
type LodiaControlPlaneInitializationStatus struct {
	// ControlPlaneInitialized is true from the first time the ControlPlane is
	// Ready and both Secrets are copied.
	// +optional
	ControlPlaneInitialized *bool `json:"controlPlaneInitialized,omitempty"`
}

func (c *LodiaControlPlane) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

func (c *LodiaControlPlane) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}
