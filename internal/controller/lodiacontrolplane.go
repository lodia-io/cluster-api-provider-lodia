// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Package controller is the LodiaControlPlane controller: it keeps one Lodia
// ControlPlane for each LodiaControlPlane and reports it to Cluster API.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/finalizers"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	"sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

const (
	Name         = "lodiacontrolplane"
	FieldManager = "cluster-api-provider-lodia"
)

// +kubebuilder:rbac:groups=controlplane.lodia.io,resources=lodiacontrolplanes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=controlplane.lodia.io,resources=lodiacontrolplanes/status,verbs=patch;update
// +kubebuilder:rbac:groups=controlplane.lodia.io,resources=lodiacontrolplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=lodia.io,resources=controlplanes,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;patch

type Reconciler struct {
	client.Client

	// APIReader reads the ControlPlane and Secrets from the API server: the
	// cache holds no Secret data, and a predecessor's object may be outside it.
	APIReader client.Reader
}

// Scheme holds the kinds the controller reads and writes.
func Scheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(lodiav1alpha1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	return scheme
}

// CacheOptions keep of Secrets only Lodia's, and only their metadata, which
// is all the controller watches.
func CacheOptions() (cache.Options, error) {
	lodias, err := labels.NewRequirement(lodiav1alpha1.ControlPlaneLabel, selection.Exists, nil)
	if err != nil {
		return cache.Options{}, err
	}
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Label: labels.NewSelector().Add(*lodias)},
		},
	}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		WithOptions(controller.Options{ReconciliationTimeout: time.Minute}).
		For(&v1alpha1.LodiaControlPlane{}).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
				return lodiaControlPlaneOf(o.(*clusterv1.Cluster))
			}),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&lodiav1alpha1.ControlPlane{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				return r.forCluster(ctx, o.GetNamespace(), o.GetName())
			})).
		WatchesMetadata(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				cp := o.GetLabels()[lodiav1alpha1.ControlPlaneLabel]
				if o.GetName() != lodiav1alpha1.AdminKubeconfigSecret.Name(cp) && o.GetName() != lodiav1alpha1.CABundleSecret.Name(cp) {
					return nil
				}
				return r.forCluster(ctx, o.GetNamespace(), cp)
			})).
		Complete(r)
}

func (r *Reconciler) forCluster(ctx context.Context, namespace, name string) []reconcile.Request {
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cluster); err != nil {
		return nil
	}
	return lodiaControlPlaneOf(cluster)
}

func lodiaControlPlaneOf(cluster *clusterv1.Cluster) []reconcile.Request {
	ref := cluster.Spec.ControlPlaneRef
	if ref.APIGroup != v1alpha1.GroupVersion.Group || ref.Kind != "LodiaControlPlane" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}}}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	lcp := &v1alpha1.LodiaControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, lcp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cluster, err := util.GetOwnerCluster(ctx, r.Client, lcp.ObjectMeta)
	if err != nil || cluster == nil {
		return ctrl.Result{}, err
	}
	if added, err := finalizers.EnsureFinalizer(ctx, r.Client, lcp, v1alpha1.Finalizer); err != nil || added {
		return ctrl.Result{}, err
	}
	patchHelper, err := patch.NewHelper(lcp, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, lcp); err != nil || isPaused || requeue {
		return ctrl.Result{}, err
	}
	defer func() {
		reterr = errors.Join(reterr, patchHelper.Patch(ctx, lcp, patch.WithOwnedConditions{Conditions: []string{
			clusterv1.ReadyCondition, clusterv1.AvailableCondition,
		}}))
	}()

	if !lcp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, cluster, lcp)
	}
	return ctrl.Result{}, r.reconcileNormal(ctx, cluster, lcp)
}

func (r *Reconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, lcp *v1alpha1.LodiaControlPlane) error {
	if _, ok := lcp.Annotations[clusterctlv1.BlockMoveAnnotation]; !ok {
		annotations.AddAnnotations(lcp, map[string]string{clusterctlv1.BlockMoveAnnotation: "true"})
		return nil
	}
	lcp.Status.ExternalManagedControlPlane = new(true)

	cp, err := r.applyControlPlane(ctx, cluster, lcp)
	if err != nil {
		return reportWriteError(lcp, err)
	}
	if err := r.copySecrets(ctx, cluster, lcp); err != nil {
		return reportWriteError(lcp, err)
	}
	return report(lcp, cp)
}

func (r *Reconciler) applyControlPlane(ctx context.Context, cluster *clusterv1.Cluster, lcp *v1alpha1.LodiaControlPlane) (*unstructured.Unstructured, error) {
	current := &lodiav1alpha1.ControlPlane{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, current); err == nil {
		if !metav1.IsControlledBy(current, lcp) {
			return nil, &controlledElsewhereError{kind: "ControlPlane", object: current}
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&lcp.Spec)
	if err != nil {
		return nil, err
	}
	delete(spec, "controlPlaneEndpoint")
	network := cluster.Spec.ClusterNetwork
	spec["network"], err = runtime.DefaultUnstructuredConverter.ToUnstructured(&lodiav1alpha1.Network{
		ServiceCIDRs:  cidrs(network.Services.CIDRBlocks),
		PodCIDRs:      cidrs(network.Pods.CIDRBlocks),
		ClusterDomain: lodiav1alpha1.Hostname(network.ServiceDomain),
	})
	if err != nil {
		return nil, err
	}

	cp := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	cp.SetGroupVersionKind(lodiav1alpha1.GroupVersion.WithKind("ControlPlane"))
	cp.SetNamespace(cluster.Namespace)
	cp.SetName(cluster.Name)
	cp.SetLabels(map[string]string{clusterv1.ClusterNameLabel: cluster.Name})
	cp.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(lcp, v1alpha1.GroupVersion.WithKind("LodiaControlPlane"))})
	return cp, r.Apply(ctx, client.ApplyConfigurationFromUnstructured(cp), client.FieldOwner(FieldManager), client.ForceOwnership)
}

func cidrs(blocks []string) []lodiav1alpha1.CIDR {
	out := make([]lodiav1alpha1.CIDR, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, lodiav1alpha1.CIDR(b))
	}
	return out
}

// copySecrets copies the CA bundle's certificates alone: the CA's key never
// leaves Lodia's PKI.
func (r *Reconciler) copySecrets(ctx context.Context, cluster *clusterv1.Cluster, lcp *v1alpha1.LodiaControlPlane) error {
	for _, c := range []struct {
		from    lodiav1alpha1.Secret
		fromKey string
		to      secret.Purpose
		toKey   string
	}{
		{lodiav1alpha1.AdminKubeconfigSecret, lodiav1alpha1.KubeconfigKey, secret.Kubeconfig, secret.KubeconfigDataName},
		{lodiav1alpha1.CABundleSecret, lodiav1alpha1.CACertKey, secret.ClusterCA, secret.TLSCrtDataName},
	} {
		source := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: c.from.Name(cluster.Name)}, source); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := r.applySecret(ctx, cluster, lcp, secret.Name(cluster.Name, c.to), map[string][]byte{c.toKey: source.Data[c.fromKey]}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) applySecret(ctx context.Context, cluster *clusterv1.Cluster, lcp *v1alpha1.LodiaControlPlane, name string, data map[string][]byte) error {
	current := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, current); err == nil {
		if !metav1.IsControlledBy(current, lcp) {
			return &controlledElsewhereError{kind: "Secret", object: current}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	owner := metav1.NewControllerRef(lcp, v1alpha1.GroupVersion.WithKind("LodiaControlPlane"))
	return r.Apply(ctx, corev1ac.Secret(name, cluster.Namespace).
		WithLabels(map[string]string{clusterv1.ClusterNameLabel: cluster.Name}).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(owner.APIVersion).WithKind(owner.Kind).WithName(owner.Name).WithUID(owner.UID).
			WithController(true).WithBlockOwnerDeletion(true)).
		WithType(clusterv1.ClusterSecretType).
		WithData(data),
		client.FieldOwner(FieldManager), client.ForceOwnership)
}

func report(lcp *v1alpha1.LodiaControlPlane, u *unstructured.Unstructured) error {
	cp := &lodiav1alpha1.ControlPlane{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, cp); err != nil {
		return err
	}
	if e := cp.Status.Endpoint; e != nil {
		lcp.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: e.APIServer.Host, Port: e.APIServer.Port}
	}
	if meta.IsStatusConditionTrue(cp.Status.Conditions, lodiav1alpha1.ConditionReady) {
		lcp.Status.Initialization.ControlPlaneInitialized = new(true)
	}
	if k := cp.Status.Kubernetes; k != nil && k.Version != "" {
		lcp.Status.Version = k.Version
	}
	if err := conditions.SetMirrorConditionFromUnstructured(u, lcp, lodiav1alpha1.ConditionReachable,
		conditions.TargetConditionType(clusterv1.AvailableCondition)); err != nil {
		return err
	}
	return conditions.SetMirrorConditionFromUnstructured(u, lcp, lodiav1alpha1.ConditionReady)
}

// reportWriteError sets Ready for a write the API server refuses or another
// controller's object blocks; the pass is retried with backoff.
func reportWriteError(lcp *v1alpha1.LodiaControlPlane, err error) error {
	var elsewhere *controlledElsewhereError
	switch {
	case errors.As(err, &elsewhere):
		conditions.Set(lcp, metav1.Condition{Type: clusterv1.ReadyCondition, Status: metav1.ConditionFalse,
			Reason: v1alpha1.ObjectsControlledElsewhereReason, Message: err.Error()})
	case apierrors.IsInvalid(err) || apierrors.IsForbidden(err):
		conditions.Set(lcp, metav1.Condition{Type: clusterv1.ReadyCondition, Status: metav1.ConditionFalse,
			Reason: v1alpha1.RefusedReason, Message: err.Error()})
	}
	return err
}

type controlledElsewhereError struct {
	kind   string
	object client.Object
}

func (e *controlledElsewhereError) Error() string {
	if owner := metav1.GetControllerOf(e.object); owner != nil {
		return fmt.Sprintf("%s %s is controlled by %s %s (uid %s)", e.kind, e.object.GetName(), owner.Kind, owner.Name, owner.UID)
	}
	return fmt.Sprintf("%s %s exists without a controller", e.kind, e.object.GetName())
}

// reconcileDelete keeps the finalizer until the ControlPlane is gone, which
// Lodia's own finalizer decides.
func (r *Reconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, lcp *v1alpha1.LodiaControlPlane) error {
	cp := &lodiav1alpha1.ControlPlane{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, cp)
	switch {
	case apierrors.IsNotFound(err) || err == nil && !metav1.IsControlledBy(cp, lcp):
		controllerutil.RemoveFinalizer(lcp, v1alpha1.Finalizer)
		return nil
	case err != nil:
		return err
	}
	if cp.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, cp, client.Preconditions{UID: &cp.UID}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	conditions.Set(lcp, metav1.Condition{Type: clusterv1.ReadyCondition, Status: metav1.ConditionFalse,
		Reason: clusterv1.DeletingReason, Message: fmt.Sprintf("waiting for ControlPlane %s to be deleted", cp.Name)})
	return nil
}
