// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
	"go.lodia.io/cluster-api-provider-lodia/test/clusterctlproxy"
)

func newClient(cfg *envconf.Config) (client.Client, error) {
	return clusterctlproxy.NewClient(cfg.Client().RESTConfig(), controller.Scheme(), clientRequestTimeout, discoveryTimeout)
}

func deploymentAvailable(c client.Reader, key client.ObjectKey) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, key, deployment); err != nil {
			return false, err
		}
		return slices.ContainsFunc(deployment.Status.Conditions, func(c appsv1.DeploymentCondition) bool {
			return c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue
		}), nil
	}
}

func createWhenWebhooksAnswer(c client.Client, bound, interval time.Duration) decoder.HandlerFunc {
	return func(ctx context.Context, obj k8s.Object) error {
		bounded, cancel := endAfter(ctx, bound)
		defer cancel()
		var last error
		err := wait.PollUntilContextCancel(bounded, interval, true, func(ctx context.Context) (bool, error) {
			err := c.Create(ctx, obj)
			switch {
			case err == nil:
				return true, nil
			case ctx.Err() != nil:
				return false, ctx.Err()
			}
			last = err
			if webhookNotReady(err) {
				return false, nil
			}
			return true, err
		})
		if wait.Interrupted(err) && last != nil {
			return last
		}
		return endedBy(bounded, err)
	}
}

func webhookNotReady(err error) bool {
	msg := err.Error()
	return apierrors.IsInternalError(err) && (strings.Contains(msg, "webhook") || strings.Contains(msg, "x509"))
}

func deploymentsAvailable(ctx context.Context, c client.Reader, namespace string, bound, interval time.Duration, names ...string) error {
	for _, name := range names {
		if err := deploymentAvailableWithin(ctx, c, client.ObjectKey{Namespace: namespace, Name: name}, bound, interval); err != nil {
			return fmt.Errorf("deployment %s/%s is not Available: %w", namespace, name, err)
		}
	}
	return nil
}

func deploymentAvailableWithin(ctx context.Context, c client.Reader, key client.ObjectKey, bound, interval time.Duration) error {
	bounded, cancel := endAfter(ctx, bound)
	defer cancel()
	return endedBy(bounded, wait.PollUntilContextCancel(bounded, interval, true, deploymentAvailable(c, key)))
}

func shortenTheDiscovery(t *testing.T, timeout time.Duration) {
	t.Helper()
	was := discoveryTimeout
	t.Cleanup(func() { discoveryTimeout = was })
	discoveryTimeout = timeout
}

func shortenTheClientRequests(t *testing.T, timeout time.Duration) {
	t.Helper()
	was := clientRequestTimeout
	t.Cleanup(func() { clientRequestTimeout = was })
	clientRequestTimeout = timeout
}

func TestTheClientsDiscoveryEndsAtTheDiscoveryTimeoutWhenTheClusterDoesNotAnswer(t *testing.T) {
	const timeout = 500 * time.Millisecond
	for name, stalled := range stalledTenants {
		t.Run(name, func(t *testing.T) {
			shortenTheDiscovery(t, timeout)
			url, _ := stalled(t)
			c, err := newClient(configFor(t, url))
			if err != nil {
				t.Fatal(err)
			}
			err = returnsWithin(t, 6*timeout, "the get", func() error { return c.Get(context.Background(), client.ObjectKey{Name: "n"}, &corev1.Node{}) })
			if err == nil {
				t.Error("the get got an answer")
			}
		})
	}
}

func TestTheClientsRequestsAskForTheirOwnTimeouts(t *testing.T) {
	shortenTheDiscovery(t, 7*time.Second)
	url, asked := recordedServer(t, tenantHandler(t))
	c, err := newClient(configFor(t, url))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(t.Context(), &corev1.NodeList{}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/api", "/apis", "/api/v1"} {
		requireRequestTimeout(t, asked(), suffix, discoveryTimeout)
	}
	requireRequestTimeout(t, asked(), "/nodes", clientRequestTimeout)
}

func TestTheClientsDiscoveryAcceptsAnAnswerWithinTheDiscoveryTimeout(t *testing.T) {
	const timeout = time.Second
	shortenTheDiscovery(t, timeout)
	url, _ := gatedTenant(t, func(string) (time.Duration, bool) { return 2 * timeout / 5, true })
	c, err := newClient(configFor(t, url))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(t.Context(), &corev1.NodeList{}); err != nil {
		t.Fatalf("discovery that answers in %s failed: %v", 2*timeout/5, err)
	}
}

func TestTheClientAcceptsACreateTheClusterHoldsPastTheDiscoveryTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	shortenTheDiscovery(t, timeout)
	shortenTheClientRequests(t, 10*timeout)
	healthy := tenantHandler(t)
	url, asked := recordedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			healthy.ServeHTTP(w, r)
			return
		}
		select {
		case <-time.After(2 * timeout):
			respond(t, w, `{"kind":"Node","apiVersion":"v1","metadata":{"name":"n"}}`)
		case <-r.Context().Done():
		}
	}))
	c, err := newClient(configFor(t, url))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}); err != nil {
		t.Fatalf("a create that the cluster held for %s failed: %v", 2*timeout, err)
	}
	requireRequestTimeout(t, asked(), "/nodes", clientRequestTimeout)
}

func TestTheClientEndsACallTheClusterHoldsAtTheClientRequestTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	shortenTheClientRequests(t, timeout)
	healthy := tenantHandler(t)
	url, asked := recordedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDiscovery(r.URL.Path) {
			healthy.ServeHTTP(w, r)
			return
		}
		<-r.Context().Done()
	}))
	c, err := newClient(configFor(t, url))
	if err != nil {
		t.Fatal(err)
	}
	err = returnsWithin(t, 6*timeout, "the get", func() error { return c.Get(context.Background(), client.ObjectKey{Name: "n"}, &corev1.Node{}) })
	if err == nil {
		t.Error("the get got an answer")
	}
	requireRequestTimeout(t, asked(), "/nodes/n", clientRequestTimeout)
}

func TestTheClientKnowsTheKindsTheStepsRead(t *testing.T) {
	server := httptest.NewTLSServer(managementHandler(t))
	t.Cleanup(server.Close)
	c, err := newClient(configFor(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(t.Context(), &clusterv1.ClusterList{}); err != nil {
		t.Fatal(err)
	}
}

func TestADeploymentIsAvailableWhenItsAvailableConditionIsTrue(t *testing.T) {
	deployment := func(name string, conditions ...appsv1.DeploymentCondition) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Status:     appsv1.DeploymentStatus{Conditions: conditions},
		}
	}
	c := fake.NewClientBuilder().WithObjects(
		deployment("available", appsv1.DeploymentCondition{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}),
		deployment("unavailable", appsv1.DeploymentCondition{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse}),
		deployment("only progressing", appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue}),
		deployment("without conditions"),
	).Build()
	for name, want := range map[string]bool{"available": true, "unavailable": false, "only progressing": false, "without conditions": false} {
		t.Run(name, func(t *testing.T) {
			done, err := deploymentAvailable(c, client.ObjectKey{Namespace: "ns", Name: name})(t.Context())
			if err != nil || done != want {
				t.Fatalf("got %v, %v, want %v", done, err, want)
			}
		})
	}
	if _, err := deploymentAvailable(c, client.ObjectKey{Namespace: "ns", Name: "missing"})(t.Context()); err == nil {
		t.Fatal("a Deployment that does not exist is not an error")
	}
}

func TestACreateIsRetriedWhileTheWebhooksDoNotAnswerAndFailsWithTheirErrorWhereTheClientsLimiterWouldHoldARead(t *testing.T) {
	webhook := apierrors.NewInternalError(errors.New(`failed calling webhook "validate.example.com": connection refused`))
	cases := []struct {
		name     string
		answers  func(n int) error
		passes   int
		wantErr  []string
		creates  int32
		maxTakes time.Duration
	}{
		{"webhooks that never answer, and a limiter that holds the third create", func(int) error { return webhook }, 2, []string{`failed calling webhook "validate.example.com"`}, 0, 0},
		{"webhooks that answer at the third create", func(n int) error {
			if n < 3 {
				return webhook
			}
			return nil
		}, 0, nil, 3, time.Second},
		{"a create that is refused", func(int) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "c", errors.New("no"))
		}, 0, []string{"forbidden"}, 1, time.Second},
		{"a create of an object that exists", func(int) error {
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "c")
		}, 0, []string{"already exists"}, 1, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var deadlined, creates atomic.Int32
			limiter := flowcontrol.NewTokenBucketRateLimiter(0.001, cmp.Or(c.passes, 1<<20))
			base := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
				Create: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
					n := creates.Add(1)
					if err := throttled(ctx, limiter, &deadlined); err != nil {
						return err
					}
					return c.answers(int(n))
				},
			})
			start := time.Now()
			err := createWhenWebhooksAnswer(base, 200*time.Millisecond, 10*time.Millisecond)(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c"}})
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
			} else {
				requireNamed(t, err, &deadlined, c.wantErr...)
			}
			if c.creates > 0 && creates.Load() != c.creates {
				t.Errorf("%d creates were made, want %d", creates.Load(), c.creates)
			}
			if took := time.Since(start); c.maxTakes > 0 && took > c.maxTakes {
				t.Errorf("the create took %s, more than %s: it ends at its first answer that is not a webhook's", took, c.maxTakes)
			}
		})
	}
}

func TestACreateThatTheClusterHoldsThroughItsBoundFailsWithTheCauseOfTheEnd(t *testing.T) {
	c := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Create: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	err := createWhenWebhooksAnswer(c, 100*time.Millisecond, 10*time.Millisecond)(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline that ended the create, got %v", err)
	}
}

func TestDeploymentsAreAvailableWhenEachBecomesAvailableWithinItsBoundAndTheFailureNamesTheOneThatDoesNotWhereTheClientsLimiterWouldHoldARead(t *testing.T) {
	deployment := func(name string, available corev1.ConditionStatus) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Status:     appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: available}}},
		}
	}
	cases := []struct {
		name    string
		names   []string
		passes  int
		wantErr []string
	}{
		{"both are Available", []string{"a", "c"}, 0, nil},
		{"the second is not, and a limiter that holds the third read", []string{"a", "b"}, 2, []string{"deployment ns/b is not Available: context deadline exceeded"}},
		{"the first is not, and a limiter that holds the second read", []string{"b", "a"}, 1, []string{"deployment ns/b is not Available: context deadline exceeded"}},
		{"one does not exist", []string{"a", "missing"}, 0, []string{"deployment ns/missing is not Available", "not found"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var deadlined atomic.Int32
			limiter := flowcontrol.NewTokenBucketRateLimiter(0.001, cmp.Or(c.passes, 1<<20))
			reads := interceptor.NewClient(fake.NewClientBuilder().WithObjects(deployment("a", corev1.ConditionTrue), deployment("b", corev1.ConditionFalse), deployment("c", corev1.ConditionTrue)).Build(), interceptor.Funcs{
				Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := throttled(ctx, limiter, &deadlined); err != nil {
						return err
					}
					return w.Get(ctx, key, obj, opts...)
				},
			})
			err := deploymentsAvailable(t.Context(), reads, "ns", 200*time.Millisecond, 10*time.Millisecond, c.names...)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			requireNamed(t, err, &deadlined, c.wantErr...)
		})
	}
}
