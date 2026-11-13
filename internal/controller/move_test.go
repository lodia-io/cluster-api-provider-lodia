// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/test/clusterctlproxy"
)

const moveBound = 20 * time.Second

func labelledAsClusterctlLabelsIt(t *testing.T, crd string) {
	t.Helper()
	labelled := &unstructured.Unstructured{}
	labelled.SetAPIVersion("apiextensions.k8s.io/v1")
	labelled.SetKind("CustomResourceDefinition")
	labelled.SetName(crd)
	patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"`+clusterctlv1.ClusterctlLabel+`":""}}}`))
	if err := k8s.Patch(t.Context(), labelled, patch); err != nil {
		t.Fatal(err)
	}
}

func TestClusterctlMoveWaitsForTheBlockOnTheLodiaControlPlane(t *testing.T) {
	clusterctlproxy.TrapTheDefaultKubeconfig(t)
	labelledAsClusterctlLabelsIt(t, "clusters.cluster.x-k8s.io")
	cluster, lcp := create(t, namespace(t), "moved")
	cluster.Status.Initialization.InfrastructureProvisioned = new(true)
	cluster.Status.Initialization.ControlPlaneInitialized = new(true)
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue,
		Reason: clusterv1.ClusterControlPlaneInitializedReason, Message: "initialized",
	})
	if err := k8s.Status().Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	untilControlPlane(t, direct(t, interceptor.Funcs{}), lcp, controlPlaneOf(cluster))
	get(t, lcp)
	uid := lcp.UID

	empty := filepath.Join(t.TempDir(), "clusterctl.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	configClient, err := config.New(t.Context(), empty)
	if err != nil {
		t.Fatal(err)
	}
	c, err := clusterctlproxy.New(t.Context(), configClient, admin, 10*time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), moveBound)
	defer cancel()
	options := clusterctl.MoveOptions{FromKubeconfig: clusterctlproxy.Kubeconfig, ToKubeconfig: clusterctlproxy.Kubeconfig, Namespace: cluster.Namespace}
	if err := clusterctlproxy.MoveUntilBlocked(ctx, c, options, "LodiaControlPlane", lcp.Name); err != nil {
		t.Fatal(err)
	}

	get(t, lcp)
	if lcp.UID != uid {
		t.Errorf("the LodiaControlPlane has the UID %s, want %s: the move went on to replace it", lcp.UID, uid)
	}
}
