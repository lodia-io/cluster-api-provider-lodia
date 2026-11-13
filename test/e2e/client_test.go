// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
