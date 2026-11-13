// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

func TestAWriteFailingMidPassIsRetried(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "eta")
	failed := false
	r := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if isSecretApply(obj, "eta-ca") && !failed {
				failed = true
				return apierrors.NewInternalError(errors.New("etcdserver: leader changed"))
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))

	if err := pass(t.Context(), r, lcp); !apierrors.IsInternalError(err) {
		t.Fatalf("the pass returned %v, want the failed write's error", err)
	}
	get(t, secretOf(cluster, secret.Kubeconfig))
	absent(t, secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
		t.Errorf("a server error reads as a verdict: %v", err)
	}
	if lcp.Spec.ControlPlaneEndpoint != (clusterv1.APIEndpoint{Host: "203.0.113.10", Port: 31443}) {
		t.Errorf("endpoint %+v, want it reported past the failed copy", lcp.Spec.ControlPlaneEndpoint)
	}
	if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
		t.Errorf("initialized before the copy: %+v", lcp.Status)
	}

	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
	get(t, secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	initialized(t, lcp)
}

func TestARestartBetweenWritesResumes(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "theta")
	untilBlockMove(t, direct(t, interceptor.Funcs{}), lcp)

	killed := errors.New("the process was killed")
	crashing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); ok {
				return killed
			}
			return c.Apply(ctx, obj, opts...)
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return killed
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return killed
		},
	})
	if err := pass(t.Context(), crashing, lcp); !errors.Is(err, killed) {
		t.Fatalf("the pass returned %v", err)
	}
	cp := controlPlaneOf(cluster)
	get(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))
	if err := pass(t.Context(), crashing, lcp); !errors.Is(err, killed) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if lcp.Status.Initialization.ControlPlaneInitialized != nil || meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition) != nil {
		t.Fatalf("the killed pass wrote the status: %+v", lcp.Status)
	}
	setReady(t, cp)

	restarted := direct(t, interceptor.Funcs{})
	if err := pass(t.Context(), restarted, lcp); err != nil {
		t.Fatal(err)
	}
	created := cp.UID
	get(t, cp)
	if cp.UID != created {
		t.Error("the restarted provider replaced the ControlPlane")
	}
	get(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if lcp.Status.Initialization.ControlPlaneInitialized == nil || !*lcp.Status.Initialization.ControlPlaneInitialized {
		t.Errorf("not initialized after the restart: %+v", lcp.Status)
	}
}

func TestATransientErrorIsRetriedWithoutAVerdict(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "iota")
	unavailable := true
	r := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "ControlPlane" && unavailable {
				unavailable = false
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	untilBlockMove(t, r, lcp)

	if err := pass(t.Context(), r, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, controlPlaneOf(cluster))
	get(t, lcp)
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition); c != nil {
		t.Errorf("a transient error set Ready: %+v", c)
	}

	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
	get(t, controlPlaneOf(cluster))
}

func TestAnExpiredDeadlineStopsThePassAndTheNextOneFinishes(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "kappa")
	slow := true
	r := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); ok && slow {
				slow = false
				<-ctx.Done()
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := pass(ctx, r, lcp); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))

	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
	get(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))
}

func TestAnExpiredDeadlineOnTheControlPlaneApplyIsReturned(t *testing.T) {
	_, lcp, cp, r, _ := readyControlPlane(t, "tau-slow")
	slow := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok {
				<-ctx.Done()
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := pass(ctx, slow, lcp); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the pass returned %v", err)
	}
	created := cp.UID
	get(t, cp)
	if cp.UID != created {
		t.Error("the ControlPlane was replaced")
	}
	passes(t, r, lcp)
}

func TestATransientErrorReadingInitializationCreatesNothing(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "omicron")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)

	unavailable := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	unavailable.Client = r.Client
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, controlPlaneOf(cluster))
}

func TestTheControllerRunsTheConfiguredWorkersAndBoundsAPass(t *testing.T) {
	options := controller.ControllerOptions(controller.Config{Concurrency: 7})
	if options.MaxConcurrentReconciles != 7 || options.ReconciliationTimeout != time.Minute {
		t.Errorf("options %+v", options)
	}
}

func TestOneTenantsBlockedWriteDoesNotHoldUpAnother(t *testing.T) {
	blockTenantA(t, 10, func(ns string) {
		second, _ := create(t, ns, "tenant-b")
		cp := controlPlaneOf(second)
		eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	})
}

func TestOneWorkerHoldsUpTheTenantBehindABlockedWrite(t *testing.T) {
	blockTenantA(t, 1, func(ns string) {
		second, _ := create(t, ns, "tenant-b")
		consistently(t, 3*time.Second, func() error {
			cp := controlPlaneOf(second)
			switch err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); {
			case err == nil:
				return errors.New("tenant B's ControlPlane exists while tenant A's write is blocked")
			case !apierrors.IsNotFound(err):
				return err
			}
			return nil
		})
	})
}

func blockTenantA(t *testing.T, workers int, whileBlocked func(ns string)) {
	t.Helper()
	c, err := client.NewWithWatch(asController, client.Options{Scheme: controller.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	blocked := make(chan bool, 1)
	ns := namespace(t)
	startManagerWith(t, controller.Config{Namespace: ns, Concurrency: workers}, func(ctrl.Manager) client.Client {
		return interceptor.NewClient(c, interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if a, ok := obj.(interface{ GetName() string }); ok && a.GetName() == "tenant-a" {
					_, deadline := ctx.Deadline()
					select {
					case blocked <- deadline:
					default:
					}
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return c.Apply(ctx, obj, opts...)
			},
		})
	})

	first, _ := create(t, ns, "tenant-a")
	select {
	case deadline := <-blocked:
		if !deadline {
			t.Error("a pass runs without a deadline")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("tenant A never reached its apply")
	}

	whileBlocked(ns)

	close(release)
	held := controlPlaneOf(first)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(held), held) })
}
