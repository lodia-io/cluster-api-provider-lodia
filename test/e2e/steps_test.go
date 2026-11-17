// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func applyTheCluster(ctx context.Context, cfg *envconf.Config, k8s client.Client, key client.ObjectKey, kubernetes string) error {
	for _, file := range []string{"cluster.yaml", "workers.yaml"} {
		manifest, err := render(file, testdataVariables(key.Name, kubernetes))
		if err != nil {
			return err
		}
		if err := apply(ctx, cfg, manifest); err != nil {
			return fmt.Errorf("apply %s: %w", file, err)
		}
	}
	provisioned := &unstructured.Unstructured{}
	provisioned.SetGroupVersionKind(devCluster)
	provisioned.SetNamespace(key.Namespace)
	provisioned.SetName(key.Name)
	patch := client.RawPatch(types.MergePatchType, []byte(`{"status":{"initialization":{"provisioned":true}}}`))
	if err := k8s.Status().Patch(ctx, provisioned, patch); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	return nil
}

func requireInitialized(ctx context.Context, k8s client.Reader, key client.ObjectKey) error {
	cluster := &clusterv1.Cluster{}
	if err := k8s.Get(ctx, key, cluster); err != nil {
		return err
	}
	if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition); c == nil || c.Status != metav1.ConditionTrue {
		return fmt.Errorf("ControlPlaneInitialized is %+v", c)
	}
	return nil
}

func kindnetOf(ctx context.Context, cfg *envconf.Config, k8s client.Reader, key client.ObjectKey) ([]byte, error) {
	cluster := &clusterv1.Cluster{}
	if err := k8s.Get(ctx, key, cluster); err != nil {
		return nil, err
	}
	management, err := kindCluster(cfg.KubeconfigFile())
	if err != nil {
		return nil, err
	}
	manifest, err := docker(ctx, "exec", management+"-control-plane", "cat", "/kind/manifests/default-cni.yaml")
	if err != nil {
		return nil, err
	}
	cni, err := template.New("cni").Parse(manifest)
	if err != nil {
		return nil, err
	}
	var rendered bytes.Buffer
	if err := cni.Execute(&rendered, struct{ PodSubnet string }{cluster.Spec.ClusterNetwork.Pods.CIDRBlocks[0]}); err != nil {
		return nil, err
	}
	return rendered.Bytes(), nil
}

func deleteTheCluster(ctx context.Context, k8s client.Client, key client.ObjectKey, workers int) ([]client.Object, error) {
	etcd := key.Name + "-etcd"
	owned := []client.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: etcd + "-" + etcd + "-0"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.ClusterCA)}},
	}
	for _, obj := range owned {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return nil, fmt.Errorf("%T %s, whose removal this step checks: %w", obj, obj.GetName(), err)
		}
	}
	containers, err := clusterContainers(ctx, key.Name)
	if err != nil {
		return nil, err
	}
	if err := requireContainers(containers, workers); err != nil {
		return nil, err
	}
	return owned, k8s.Delete(ctx, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})
}

func requireGone(ctx context.Context, k8s client.Reader, key client.ObjectKey, owned []client.Object) error {
	for _, obj := range append([]client.Object{
		&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
		&v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
		&lodiav1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
	}, owned...) {
		if err := gone(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)); err != nil {
			return fmt.Errorf("%T %s: %w", obj, obj.GetName(), err)
		}
	}
	left, err := clusterContainers(ctx, key.Name)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("containers %s are left", strings.Join(left, ", "))
	}
	return nil
}
