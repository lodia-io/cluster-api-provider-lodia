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
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io="
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
// sets, controlPlaneEndpoint and restoreFrom.
// +kubebuilder:validation:XValidation:rule="!has(self.highAvailability) || !has(self.apiServer) || !has(self.apiServer.replicas) || self.apiServer.replicas >= 2",message="highAvailability runs at least two API servers"
// +kubebuilder:validation:XValidation:rule="!has(self.apiServer) || !has(self.apiServer.advertiseAddress) || (has(self.endpoint.gateway) || has(self.endpoint.loadBalancer) && (!has(self.endpoint.loadBalancer.host) || !isIP(self.endpoint.loadBalancer.host)))",message="apiServer.advertiseAddress is set only where apiserver-proxy runs: with endpoint.gateway, or with endpoint.loadBalancer unless its host is an IP address"
// +kubebuilder:validation:XValidation:rule="!(has(self.endpoint.gateway) || has(self.endpoint.loadBalancer) && (!has(self.endpoint.loadBalancer.host) || !isIP(self.endpoint.loadBalancer.host))) || (has(self.apiServer) && has(self.apiServer.advertiseAddress) ? has(oldSelf.apiServer) && has(oldSelf.apiServer.advertiseAddress) && oldSelf.apiServer.advertiseAddress == self.apiServer.advertiseAddress : !has(oldSelf.apiServer) || !has(oldSelf.apiServer.advertiseAddress))",message="apiServer.advertiseAddress is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.datastore) && has(self.datastore.className) && size(self.datastore.className) > 0 ? has(oldSelf.datastore) && has(oldSelf.datastore.className) && oldSelf.datastore.className == self.datastore.className : !has(oldSelf.datastore) || !has(oldSelf.datastore.className) || size(oldSelf.datastore.className) == 0",message="datastore.className is immutable in v1alpha1"
// +kubebuilder:validation:XValidation:rule="!has(self.serviceAccount) || !has(self.serviceAccount.issuer) || !has(self.apiServer) || !has(self.apiServer.authentication) || !has(self.apiServer.authentication.jwt) || !self.apiServer.authentication.jwt.exists(a, a.issuer.url == self.serviceAccount.issuer)",message="kube-apiserver refuses a JWT issuer.url equal to serviceAccount.issuer"
type LodiaControlPlaneTemplateResourceSpec struct {
	// HighAvailability runs every component with more than one replica,
	// spread by FailureTolerance, and etcd with three members.
	// +optional
	HighAvailability *lodiav1alpha1.HighAvailability `json:"highAvailability,omitempty"`

	// Datastore is where the cluster's data lives: an etcd of the tenant's
	// own.
	// +optional
	Datastore lodiav1alpha1.DatastoreSettings `json:"datastore,omitzero"`

	// Endpoint is how nodes, people and pods reach the API server.
	// +required
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

	// Health configures how Lodia protects the tenant's workloads.
	// +optional
	Health *lodiav1alpha1.Health `json:"health,omitempty"`

	// ServiceAccount configures the tokens the API server issues.
	// +optional
	ServiceAccount *lodiav1alpha1.ServiceAccount `json:"serviceAccount,omitempty"`
}
