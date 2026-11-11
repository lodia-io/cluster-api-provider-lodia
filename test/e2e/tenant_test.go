// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

type tenantAccess struct {
	cfg       *rest.Config
	client    client.Client
	resources *resources.Resources
}

func tenantRESTConfig(ctx context.Context, k8s client.Reader, key client.ObjectKey, timeout time.Duration) (*rest.Config, error) {
	kubeconfig := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)}, kubeconfig); err != nil {
		return nil, err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig.Data[secret.KubeconfigDataName])
	if err != nil {
		return nil, err
	}
	cfg.Timeout = timeout
	return cfg, nil
}

func openTenant(ctx context.Context, t *testing.T, k8s client.Client, key client.ObjectKey, timeout time.Duration) *tenantAccess {
	t.Helper()
	cfg, err := tenantRESTConfig(ctx, k8s, key, timeout)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := resources.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the tenant's API server %s is reached through %s", cfg.Host, secret.Name(key.Name, secret.Kubeconfig))
	return &tenantAccess{cfg: cfg, client: c, resources: r}
}

func tenantThatNeverCompletesTLS(t *testing.T) (string, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return "https://" + ln.Addr().String(), func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(conns)
	}
}

func gatedTenant(t *testing.T, gate func(path string) (delay time.Duration, answers bool)) (string, func() []string) {
	t.Helper()
	healthy := tenantHandler(t)
	var (
		mu    sync.Mutex
		asked []string
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		delay, answers := gate(r.URL.Path)
		if !answers {
			<-r.Context().Done()
			return
		}
		select {
		case <-time.After(delay):
			healthy.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func tenantThatNeverAnswers(t *testing.T) (string, func() int) {
	t.Helper()
	url, asked := gatedTenant(t, func(string) (time.Duration, bool) { return 0, false })
	return url, func() int { return len(asked()) }
}

func tenantThatNeverAnswersApis(t *testing.T) (string, func() int) {
	t.Helper()
	url, asked := gatedTenant(t, func(path string) (time.Duration, bool) { return 0, path == "/api" })
	return url, func() int { return countPaths(asked(), func(path string) bool { return path == "/apis" }) }
}

var stalledTenants = map[string]func(*testing.T) (string, func() int){
	"a tenant that accepts connections and never completes TLS": tenantThatNeverCompletesTLS,
	"a tenant that completes TLS and never answers":             tenantThatNeverAnswers,
	"a tenant that answers /api and never /apis":                tenantThatNeverAnswersApis,
}

func isDiscovery(path string) bool {
	return slices.Contains([]string{"/api", "/apis", "/api/v1", "/apis/apps/v1"}, path)
}

func countPaths(paths []string, match func(string) bool) int {
	var n int
	for _, path := range paths {
		if match(path) {
			n++
		}
	}
	return n
}

func returnsWithin(t *testing.T, limit time.Duration, what string, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("%s is still running after %s", what, limit)
		return nil
	}
}

func shortenTheDump(t *testing.T, dump, tenant time.Duration) {
	t.Helper()
	was, wasTenant := bounds.Dump, tenantDumpBound
	t.Cleanup(func() { bounds.Dump, tenantDumpBound = was, wasTenant })
	bounds.Dump, tenantDumpBound = dump, tenant
}

func TestTheDumpOfATenantThatDoesNotAnswerEndsInItsBoundAndLeavesTimeForTheWorkers(t *testing.T) {
	const bound = 1500 * time.Millisecond
	for name, stalled := range stalledTenants {
		t.Run(name, func(t *testing.T) {
			shortenTheDump(t, 4*bound, bound)
			url, stalls := stalled(t)
			d := newDumpSetup(t, url)
			err := returnsWithin(t, bound+bound/2, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) })
			if err == nil {
				t.Fatal("the dump says nothing about a tenant that did not answer")
			}
			requireFiles(t, d.dir, map[string]string{"worker/worker/journal.log": "journalctl"})
			if n := stalls(); n <= len(tenantObjects)/2 {
				t.Errorf("only %d of the %d lists reached the tenant: each should be cut off after a sixth of its bound", n, len(tenantObjects))
			}
		})
	}
}

func TestEveryCallToATenantThatDoesNotAnswerFails(t *testing.T) {
	const timeout = 250 * time.Millisecond
	key := client.ObjectKey{Namespace: namespace, Name: "c-w"}
	for name, stalled := range stalledTenants {
		t.Run(name, func(t *testing.T) {
			url, _ := stalled(t)
			k8s := fake.NewClientBuilder().WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)},
				Data:       map[string][]byte{secret.KubeconfigDataName: serverKubeconfig(t, url, "tenant")},
			}).Build()
			tenant := openTenant(t.Context(), t, k8s, key, timeout)
			if tenant.cfg.Timeout != timeout {
				t.Fatalf("the tenant's rest config has the timeout %s, want %s", tenant.cfg.Timeout, timeout)
			}
			for what, call := range map[string]func() error{
				"the client": func() error { return tenant.client.Get(t.Context(), client.ObjectKey{Name: "n"}, &corev1.Node{}) },
				"resources":  func() error { return tenant.resources.Get(t.Context(), "n", "", &corev1.Node{}) },
				"clientset": func() error {
					_, err := kubernetes.NewForConfigOrDie(tenant.cfg).CoreV1().Pods(metav1.NamespaceDefault).Get(t.Context(), "web", metav1.GetOptions{})
					return err
				},
			} {
				if err := returnsWithin(t, 3*time.Second, what, call); err == nil {
					t.Errorf("%s got an answer", what)
				}
			}
		})
	}
}

func TestTheDumpStopsDiscoveringOnceTheTenantsBoundHasEnded(t *testing.T) {
	const bound = 1500 * time.Millisecond
	request := bound / time.Duration(len(tenantObjects))
	url, asked := gatedTenant(t, func(path string) (time.Duration, bool) { return request * 9 / 10, path == "/api" })
	d := newDumpSetup(t, url)
	if err := dumpTenant(t.Context(), d.cfg, d.c, d.dir, bound); err == nil {
		t.Fatal("the dump says nothing about a tenant that never answered /apis")
	}
	if n := countPaths(asked(), func(path string) bool { return path == "/apis" }); n >= len(tenantObjects) {
		t.Fatalf("%d of the %d lists went on to discover after the tenant's bound", n, len(tenantObjects))
	}
}

func TestTheDumpStopsListingATenantAtItsBound(t *testing.T) {
	const bound = 1500 * time.Millisecond
	half := bound / time.Duration(len(tenantObjects)) / 2
	url, asked := gatedTenant(t, func(path string) (time.Duration, bool) { return half, isDiscovery(path) })
	d := newDumpSetup(t, url)
	if err := dumpTenant(t.Context(), d.cfg, d.c, d.dir, bound); err == nil {
		t.Fatal("the dump says nothing about a tenant that never answered a list")
	}
	if n := countPaths(asked(), func(path string) bool { return !isDiscovery(path) }); n >= len(tenantObjects) {
		t.Fatalf("%d of the %d lists reached the tenant, which cannot have happened inside its bound", n, len(tenantObjects))
	}
}

func TestTheDumpOfATenantWhoseBoundHasEndedSaysSo(t *testing.T) {
	d := newDumpSetup(t, tenantAPI(t).URL)
	err := dumpTenant(t.Context(), d.cfg, d.c, d.dir, time.Nanosecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline of the tenant's bound, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "tenant")); !os.IsNotExist(err) {
		t.Fatalf("the dump listed a tenant after its bound: %v", err)
	}
}

func TestTheDumpReadsTheTenantsKubeconfigWithinItsBound(t *testing.T) {
	const bound = 500 * time.Millisecond
	d := newDumpSetup(t, tenantAPI(t).URL)
	management := interceptor.NewClient(d.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
				return nil
			}
		},
	})
	err := returnsWithin(t, 2*bound, "the dump of the tenant", func() error { return dumpTenant(t.Context(), d.cfg, management, d.dir, bound) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline of the tenant's bound, got %v", err)
	}
}
