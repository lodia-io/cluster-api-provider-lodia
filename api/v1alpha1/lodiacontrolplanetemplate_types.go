// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// LodiaControlPlaneTemplate is a LodiaControlPlane for a ClusterClass.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=lodiacontrolplanetemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
type LodiaControlPlaneTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec LodiaControlPlaneTemplateSpec `json:"spec"`
}

// LodiaControlPlaneTemplateList is a list of LodiaControlPlaneTemplates.
// +kubebuilder:object:root=true
type LodiaControlPlaneTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LodiaControlPlaneTemplate `json:"items"`
}

// LodiaControlPlaneTemplateSpec holds the template.
type LodiaControlPlaneTemplateSpec struct {
	// +required
	Template LodiaControlPlaneTemplateResource `json:"template"`
}

// LodiaControlPlaneTemplateResource is the metadata and spec of the
// LodiaControlPlanes made from the template.
type LodiaControlPlaneTemplateResource struct {
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// +required
	Spec LodiaControlPlaneTemplateResourceSpec `json:"spec"`
}

// LodiaControlPlaneTemplateResourceSpec is the part of a LodiaControlPlane's
// spec clusters can share: all but the version, which the Cluster's topology
// sets, the endpoint and restoreFrom.
// +kubebuilder:validation:XValidation:rule="self.layout != 'Compact' || !has(self.highAvailability)",message="the Compact layout runs one replica of everything: remove highAvailability"
// +kubebuilder:validation:XValidation:rule="self.layout != 'Compact' || !has(self.apiServer) || !has(self.apiServer.replicas) || self.apiServer.replicas == 1",message="the Compact layout runs one API server"
type LodiaControlPlaneTemplateResourceSpec struct {
	// Layout is Split, a Deployment per component, or Compact, every
	// component in one pod.
	// +optional
	// +kubebuilder:default=Split
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="layout is immutable"
	Layout lodiav1alpha1.Layout `json:"layout,omitempty"`

	// HighAvailability runs every component with more than one replica,
	// spread by FailureTolerance, and etcd with three members.
	// +optional
	HighAvailability *lodiav1alpha1.HighAvailability `json:"highAvailability,omitempty"`

	// Datastore is where the cluster's data lives.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="datastore is immutable in v1alpha1"
	Datastore lodiav1alpha1.DatastoreChoice `json:"datastore"`

	// Endpoint is how nodes, people and pods reach the API server.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="endpoint is immutable in v1alpha1"
	Endpoint lodiav1alpha1.Endpoint `json:"endpoint"`

	// APIServer configures kube-apiserver.
	// +optional
	APIServer *lodiav1alpha1.APIServer `json:"apiServer,omitempty"`

	// ControllerManager configures kube-controller-manager.
	// +optional
	ControllerManager *lodiav1alpha1.Component `json:"controllerManager,omitempty"`

	// Scheduler configures kube-scheduler.
	// +optional
	Scheduler *lodiav1alpha1.Scheduler `json:"scheduler,omitempty"`

	// Addons configures what Lodia runs inside the tenant cluster.
	// +optional
	Addons *lodiav1alpha1.Addons `json:"addons,omitempty"`

	// Placement says where this control plane's pods run.
	// +optional
	Placement *lodiav1alpha1.Placement `json:"placement,omitempty"`

	// Maintenance is when Lodia may roll changes it initiates.
	// +optional
	Maintenance *lodiav1alpha1.Maintenance `json:"maintenance,omitempty"`

	// Hibernation puts the control plane to sleep, keeping its data.
	// +optional
	Hibernation *lodiav1alpha1.Hibernation `json:"hibernation,omitempty"`

	// Health configures how Lodia protects the tenant's workloads.
	// +optional
	Health *lodiav1alpha1.Health `json:"health,omitempty"`

	// ServiceAccount configures the tokens the API server issues.
	// +optional
	ServiceAccount *lodiav1alpha1.ServiceAccount `json:"serviceAccount,omitempty"`
}
