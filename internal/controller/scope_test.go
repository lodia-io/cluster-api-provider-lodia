// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"errors"
	"fmt"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

func untouched(t *testing.T, lcp *v1alpha1.LodiaControlPlane) error {
	t.Helper()
	get(t, lcp)
	if _, ok := lcp.Annotations[clusterctlv1.BlockMoveAnnotation]; ok || len(lcp.Finalizers) != 0 {
		return fmt.Errorf("%s was reconciled: annotations %v, finalizers %v", lcp.Name, lcp.Annotations, lcp.Finalizers)
	}
	return nil
}

func TestTheNamespaceKeepsTheManagerFromAnotherNamespacesLodiaControlPlanes(t *testing.T) {
	ns, elsewhere := namespace(t), namespace(t)
	startManager(t, ns)
	_, outside := create(t, elsewhere, "outside")
	inside, _ := create(t, ns, "inside")
	eventually(t, func() error {
		return k8s.Get(t.Context(), client.ObjectKeyFromObject(inside), controlPlaneOf(inside))
	})

	consistently(t, func() error { return untouched(t, outside) })
}

func TestTheWatchFilterReconcilesOnlyTheObjectsCarryingItsValue(t *testing.T) {
	const mine = "mine"
	ns := namespace(t)
	startManagerWith(t, controller.Config{Namespace: ns, WatchFilterValue: mine, Concurrency: 10})
	labelled := func(value string) func(*clusterv1.Cluster, *v1alpha1.LodiaControlPlane) {
		return func(cluster *clusterv1.Cluster, lcp *v1alpha1.LodiaControlPlane) {
			cluster.Labels = map[string]string{clusterv1.WatchLabel: value}
			lcp.Labels[clusterv1.WatchLabel] = value
		}
	}

	var strangers []*v1alpha1.LodiaControlPlane
	for _, s := range []struct {
		name   string
		mutate []func(*clusterv1.Cluster, *v1alpha1.LodiaControlPlane)
		then   func(*clusterv1.Cluster)
	}{
		{"unlabelled", nil, nil},
		{"other-value", []func(*clusterv1.Cluster, *v1alpha1.LodiaControlPlane){labelled("other")}, nil},
		{"controlplane-event", nil, func(cluster *clusterv1.Cluster) { leftoverOfAnotherOwner(t, ns, cluster.Name) }},
		{"secret-event", []func(*clusterv1.Cluster, *v1alpha1.LodiaControlPlane){labelled("other")}, func(cluster *clusterv1.Cluster) { lodiaSecrets(t, cluster, nil) }},
	} {
		cluster, lcp := create(t, ns, s.name, s.mutate...)
		paused := cluster.DeepCopy()
		paused.Spec.Paused = new(true)
		if err := k8s.Patch(t.Context(), paused, client.MergeFrom(cluster)); err != nil {
			t.Fatal(err)
		}
		if s.then != nil {
			s.then(cluster)
		}
		strangers = append(strangers, lcp)
	}

	cluster, lcp := create(t, ns, mine, labelled(mine))
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	setReady(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))
	eventually(t, func() error {
		get(t, lcp)
		if i := lcp.Status.Initialization.ControlPlaneInitialized; i == nil || !*i {
			return fmt.Errorf("the events of the ControlPlane and the Secrets did not reach the manager: %+v", lcp.Status.Initialization)
		}
		return nil
	})

	consistently(t, func() error {
		for _, stranger := range strangers {
			if err := untouched(t, stranger); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestTheWatchFilterLeavesAnUnlabelledLodiaControlPlaneToItsClustersEvents(t *testing.T) {
	const mine = "mine"
	ns := namespace(t)
	cluster, lcp := create(t, ns, "unlabelled", func(cluster *clusterv1.Cluster, _ *v1alpha1.LodiaControlPlane) {
		cluster.Labels = map[string]string{clusterv1.WatchLabel: mine}
	})
	startManagerWith(t, controller.Config{Namespace: ns, WatchFilterValue: mine, Concurrency: 10})

	eventually(t, func() error {
		get(t, lcp)
		if len(lcp.Finalizers) == 0 {
			return errors.New("the start of the manager did not reach it through its Cluster")
		}
		return nil
	})
	touched := cluster.DeepCopy()
	touched.Annotations = map[string]string{"touched": "true"}
	if err := k8s.Patch(t.Context(), touched, client.MergeFrom(cluster)); err != nil {
		t.Fatal(err)
	}
	consistently(t, func() error {
		get(t, lcp)
		if condition(lcp, clusterv1.PausedCondition) != nil {
			return errors.New("its own events or a change of its Cluster's metadata reached it")
		}
		return nil
	})

	unpaused := cluster.DeepCopy()
	unpaused.Spec.Paused = new(false)
	if err := k8s.Patch(t.Context(), unpaused, client.MergeFrom(cluster)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		get(t, lcp)
		if condition(lcp, clusterv1.PausedCondition) == nil {
			return errors.New("a change of its Cluster's spec did not reach it")
		}
		return nil
	})
}
