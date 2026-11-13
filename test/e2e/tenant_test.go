// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func newTenantAccess(ctx context.Context, k8s client.Reader, key client.ObjectKey, timeout time.Duration) (*tenantAccess, error) {
	cfg, err := tenantRESTConfig(ctx, k8s, key, timeout)
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, err
	}
	r, err := resources.New(cfg)
	if err != nil {
		return nil, err
	}
	return &tenantAccess{cfg: cfg, client: c, resources: r}, nil
}

func openTenant(ctx context.Context, t *testing.T, k8s client.Client, key client.ObjectKey, timeout time.Duration) *tenantAccess {
	t.Helper()
	tenant, err := newTenantAccess(ctx, k8s, key, timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the tenant's API server %s is reached through %s", tenant.cfg.Host, secret.Name(key.Name, secret.Kubeconfig))
	return tenant
}

// Cluster API reads <cluster>-kubeconfig only when its ClusterCache connects
// and keeps the connection until its own credential is refused, so a Secret
// rewritten after the first join reaches the drain and the Node's deletion
// only on a reconnect. Opening the tenant again is what reads the Secret as the
// provider holds it now, and the read proves that its credential authenticates.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controllers/clustercache/cluster_cache.go#L496-L507
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controllers/clustercache/cluster_accessor.go#L327-L342
func reopenTenant(ctx context.Context, k8s client.Reader, key client.ObjectKey) (*tenantAccess, error) {
	tenant, err := newTenantAccess(ctx, k8s, key, tenantRequestTimeout)
	if err != nil {
		return nil, err
	}
	if err := tenant.client.List(ctx, &corev1.NodeList{}, client.Limit(1)); err != nil {
		return nil, fmt.Errorf("the tenant does not accept the credential of Secret %s: %w", secret.Name(key.Name, secret.Kubeconfig), err)
	}
	return tenant, nil
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
	return gatedServer(t, tenantHandler(t), gate)
}

func gatedServer(t *testing.T, healthy http.Handler, gate func(path string) (delay time.Duration, answers bool)) (string, func() []string) {
	t.Helper()
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

func shortenTheDump(t *testing.T, dump, tenant, workers time.Duration) {
	t.Helper()
	was, wasTenant, wasWorkers := bounds.Dump, tenantDumpBound, workerDumpBound
	t.Cleanup(func() { bounds.Dump, tenantDumpBound, workerDumpBound = was, wasTenant, wasWorkers })
	bounds.Dump, tenantDumpBound, workerDumpBound = dump, tenant, workers
}

func TestTheDumpOfATenantThatDoesNotAnswerEndsInItsBoundAndWritesTheWorkers(t *testing.T) {
	const bound = 1500 * time.Millisecond
	for name, stalled := range stalledTenants {
		t.Run(name, func(t *testing.T) {
			shortenTheDump(t, 4*bound, bound, bound)
			url, stalls := stalled(t)
			d := newDumpSetup(t, url)
			err := returnsWithin(t, bound+bound/2, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) })
			if err == nil {
				t.Fatal("the dump says nothing about a tenant that did not answer")
			}
			requireFiles(t, d.dir, map[string]string{"worker/worker/journal.log": "journalctl"})
			if n := stalls(); n <= len(tenantObjects)/2 {
				t.Errorf("only %d of the %d lists reached the tenant: each should be cut off after 1/%d of its bound", n, len(tenantObjects), len(tenantObjects))
			}
		})
	}
}

func tenantThatAccepts(t *testing.T, token string) string {
	t.Helper()
	healthy := tenantHandler(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		healthy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestTheTenantIsOpenedAgainFromTheSecretAsTheProviderHoldsItNow(t *testing.T) {
	key := client.ObjectKey{Namespace: namespace, Name: "c-w"}
	name := secret.Name(key.Name, secret.Kubeconfig)
	listNodes := func(tenant *tenantAccess) error { return tenant.client.List(t.Context(), &corev1.NodeList{}) }
	kubeconfigOf := func(server, token string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: name},
			Data:       map[string][]byte{secret.KubeconfigDataName: kubeconfigWithToken(t, server, "tenant", token)},
		}
	}

	t.Run("a Secret whose credential the server accepts", func(t *testing.T) {
		server := tenantThatAccepts(t, "good")
		tenant, err := reopenTenant(t.Context(), fake.NewClientBuilder().WithObjects(kubeconfigOf(server, "good")).Build(), key)
		if err != nil {
			t.Fatal(err)
		}
		if err := listNodes(tenant); err != nil {
			t.Errorf("the tenant that was opened again does not answer: %v", err)
		}
	})

	t.Run("a Secret rewritten with a credential the server refuses, after a tenant was opened from it", func(t *testing.T) {
		server := tenantThatAccepts(t, "good")
		k8s := fake.NewClientBuilder().WithObjects(kubeconfigOf(server, "good")).Build()
		before, err := reopenTenant(t.Context(), k8s, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := k8s.Update(t.Context(), kubeconfigOf(server, "revoked")); err != nil {
			t.Fatal(err)
		}
		if err := listNodes(before); err != nil {
			t.Fatalf("the tenant that was opened before the rewrite should keep its credential: %v", err)
		}
		_, err = reopenTenant(t.Context(), k8s, key)
		if !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), name) {
			t.Fatalf("want an Unauthorized error that names Secret %s, got %v", name, err)
		}
	})

	t.Run("a Secret rewritten from a credential the server refuses to one it accepts", func(t *testing.T) {
		server := tenantThatAccepts(t, "second")
		k8s := fake.NewClientBuilder().WithObjects(kubeconfigOf(server, "first")).Build()
		if _, err := reopenTenant(t.Context(), k8s, key); err == nil {
			t.Fatal("the credential that the server refuses was accepted")
		}
		if err := k8s.Update(t.Context(), kubeconfigOf(server, "second")); err != nil {
			t.Fatal(err)
		}
		if _, err := reopenTenant(t.Context(), k8s, key); err != nil {
			t.Fatalf("the rewritten Secret was not read again: %v", err)
		}
	})

	t.Run("no Secret", func(t *testing.T) {
		if _, err := reopenTenant(t.Context(), fake.NewClientBuilder().Build(), key); !apierrors.IsNotFound(err) {
			t.Fatalf("want a NotFound, got %v", err)
		}
	})
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
	url, asked := gatedTenant(t, func(string) (time.Duration, bool) { return 0, true })
	d := newDumpSetup(t, url)
	err := dumpTenant(t.Context(), d.cfg, d.c, d.dir, time.Nanosecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline of the tenant's bound, got %v", err)
	}
	if got := asked(); len(got) > 0 {
		t.Fatalf("the dump sent %v to a tenant after its bound", got)
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

func shortenTheDumpRequests(t *testing.T, timeout time.Duration) {
	t.Helper()
	was := dumpRequestTimeout
	t.Cleanup(func() { dumpRequestTimeout = was })
	dumpRequestTimeout = timeout
}

func TestTheDumpOfAManagementClusterThatDoesNotAnswerEndsInItsBoundAndWritesTheWorkers(t *testing.T) {
	const bound = 4 * time.Second
	shortenTheDump(t, bound, bound/8, bound)
	shortenTheDumpRequests(t, bound/4)
	useDocker(t, fakeDocker)
	stalled, _ := tenantThatNeverAnswers(t)
	cfg := configFor(t, stalled)
	dir := t.TempDir()
	if err := returnsWithin(t, 2*bound, "the dump", func() error { return dumpCluster(t.Context(), cfg, dir) }); err == nil {
		t.Fatal("the dump says nothing about a management cluster that did not answer")
	}
	requireFiles(t, dir, workerFiles)
}

func TestTheDumpOfADockerThatHangsEndsTheWorkersAtTheirBoundAndDumpsTheRest(t *testing.T) {
	const bound = 1500 * time.Millisecond
	for _, verb := range []string{"ps", "exec"} {
		t.Run("docker "+verb, func(t *testing.T) {
			shortenTheDump(t, 3*bound/2, 2*bound, bound)
			d := newDumpSetup(t, tenantAPI(t).URL)
			useDocker(t, dockerThatHangsOn(verb))
			start := time.Now()
			if err := returnsWithin(t, bound+bound/2, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) }); err == nil {
				t.Fatal("the dump says nothing about a docker that hung")
			}
			if took := time.Since(start); took < bound {
				t.Errorf("the dump ended after %s, before the workers' bound %s", took, bound)
			}
			want := map[string]string{"tenant/objects/Node.yaml": "/api/v1/nodes"}
			for _, ns := range managers {
				want["objects/"+ns+"/Event.yaml"] = "event-in-" + ns
			}
			requireFiles(t, d.dir, want)
		})
	}
}

func TestTheDumpOfADockerThatHangsAndAManagementClusterThatDoesNotAnswerEndsInItsBound(t *testing.T) {
	const bound = 3 * time.Second
	shortenTheDump(t, bound, bound/8, bound/2)
	shortenTheDumpRequests(t, bound/30)
	useDocker(t, dockerThatHangsOn("ps"))
	stalled, _ := tenantThatNeverAnswers(t)
	cfg := configFor(t, stalled)
	if err := returnsWithin(t, bound+bound/4, "the dump", func() error { return dumpCluster(t.Context(), cfg, t.TempDir()) }); err == nil {
		t.Fatal("the dump says nothing about a docker that hung and a management cluster that did not answer")
	}
}

func TestTheDumpOfADockerThatHangsAndATenantThatDoesNotAnswerEndsInItsBound(t *testing.T) {
	const (
		workers = 1500 * time.Millisecond
		dump    = 3 * workers / 2
		tenant  = 4 * workers
	)
	shortenTheDump(t, dump, tenant, workers)
	stalled, _ := tenantThatNeverAnswers(t)
	d := newDumpSetup(t, stalled)
	useDocker(t, dockerThatHangsOn("ps"))
	if err := returnsWithin(t, dump+tenant/2, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) }); err == nil {
		t.Fatal("the dump says nothing about a docker that hung and a tenant that did not answer")
	}
}

func TestTheDumpCutsOffWhatTheManagementClusterDoesNotAnswerAtItsBound(t *testing.T) {
	const bound = time.Second
	for name, stalls := range map[string]string{
		"a list of Clusters":      "/clusters",
		"a list of Pods":          "/pods",
		"the logs of a container": "/log",
	} {
		t.Run(name, func(t *testing.T) {
			shortenTheDump(t, bound, bound/4, bound/4)
			shortenTheDumpRequests(t, 10*bound)
			useDocker(t, fakeDocker)
			url, _ := gatedServer(t, managementHandler(t), func(path string) (time.Duration, bool) { return 0, !strings.HasSuffix(path, stalls) })
			cfg := configFor(t, url)
			if err := returnsWithin(t, 2*bound, "the dump", func() error { return dumpCluster(t.Context(), cfg, t.TempDir()) }); err == nil {
				t.Fatal("the dump says nothing about a request that did not answer")
			}
		})
	}
}

func managementThatHolds(t *testing.T, suffix string) dumpSetup {
	t.Helper()
	d := newDumpSetup(t, tenantAPI(t).URL)
	url, _ := gatedServer(t, managementHandler(t), func(path string) (time.Duration, bool) { return 0, !strings.HasSuffix(path, suffix) })
	d.cfg = configFor(t, url)
	return d
}

func TestTheDumpOfAKubeletThatDoesNotAnswerItsLogsWritesTheTenantFirst(t *testing.T) {
	const bound = 3 * time.Second
	shortenTheDump(t, bound, bound, bound)
	shortenTheDumpRequests(t, bound/5)
	d := managementThatHolds(t, "/log")
	start := time.Now()
	if err := returnsWithin(t, bound+bound/2, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) }); err == nil {
		t.Fatal("the dump says nothing about logs that were never answered")
	}
	if took := time.Since(start); took < bound {
		t.Errorf("the dump ended after %s, before its bound %s: the held logs did not use it up", took, bound)
	}
	requireFiles(t, d.dir, map[string]string{
		"tenant/objects/Node.yaml":          "/api/v1/nodes",
		"tenant/objects/default/Event.yaml": "/api/v1/namespaces/default/events",
	})
}

func TestTheDumpEndsAHeldListOfPodsOfTheLogsAtTheRequestTimeout(t *testing.T) {
	const (
		bound   = 6 * time.Second
		request = bound / 15
	)
	shortenTheDump(t, bound, bound, bound)
	shortenTheDumpRequests(t, request)
	d := managementThatHolds(t, "/pods")
	err := returnsWithin(t, 12*request, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) })
	if err == nil {
		t.Fatal("the dump says nothing about lists of Pods that were never answered")
	}
	for _, ns := range managers {
		if !strings.Contains(err.Error(), "/namespaces/"+ns+"/pods") {
			t.Errorf("the dump's error names no failed list of the Pods of %s: %v", ns, err)
		}
	}
}

func TestTheDumpCutsOffAHeldListOfPodsOfTheLogsAtItsBound(t *testing.T) {
	const bound = time.Second
	shortenTheDump(t, bound, bound, bound)
	shortenTheDumpRequests(t, 10*bound)
	d := managementThatHolds(t, "/pods")
	if err := returnsWithin(t, 2*bound, "the dump", func() error { return dumpBounded(t.Context(), d.cfg, d.c, d.dir) }); err == nil {
		t.Fatal("the dump says nothing about a list of Pods that was never answered")
	}
}

func TestTheDumpAsksTheManagementClusterNothingOnceItsBoundHasEnded(t *testing.T) {
	withoutDocker(t)
	url, asked := gatedServer(t, managementHandler(t), func(string) (time.Duration, bool) { return 0, true })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := dumpCluster(ctx, configFor(t, url), t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the context's error, got %v", err)
	}
	if got := asked(); len(got) > 0 {
		t.Fatalf("the dump sent %v to the management cluster after its bound", got)
	}
}

type request struct{ path, timeout string }

func recordedServer(t *testing.T, healthy http.Handler) (string, func() []request) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []request
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, request{r.URL.Path, r.URL.Query().Get("timeout")})
		mu.Unlock()
		healthy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server.URL, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func requestsOfTheDump(t *testing.T) []request {
	t.Helper()
	shortenTheDumpRequests(t, 7*time.Second)
	useDocker(t, fakeDocker)
	url, asked := recordedServer(t, managementHandler(t))
	_ = dumpCluster(t.Context(), configFor(t, url), t.TempDir())
	return asked()
}

func requireRequestTimeout(t *testing.T, requests []request, suffix string, want time.Duration) {
	t.Helper()
	var n int
	for _, r := range requests {
		if !strings.HasSuffix(r.path, suffix) {
			continue
		}
		n++
		if r.timeout != want.String() {
			t.Errorf("%s was sent with the timeout %q, want %q", r.path, r.timeout, want)
		}
	}
	if n == 0 {
		t.Errorf("the dump sent no request to a path ending in %s: %v", suffix, requests)
	}
}

func TestTheDumpGivesTheLogsOfTheManagersTheRequestTimeout(t *testing.T) {
	requireRequestTimeout(t, requestsOfTheDump(t), "/log", dumpRequestTimeout)
}

func TestTheDumpWritesTheLogsOfEveryManager(t *testing.T) {
	useDocker(t, fakeDocker)
	url, _ := recordedServer(t, managementHandler(t))
	dir := t.TempDir()
	err := dumpCluster(t.Context(), configFor(t, url), dir)
	want := make(map[string]string)
	for _, ns := range managers {
		log := "log of /api/v1/namespaces/" + ns + "/pods/p/log?"
		want[filepath.Join(ns, "p-c.log")] = log + "container=c"
		want[filepath.Join(ns, "p-c.previous.log")] = log + "container=c&previous=true"
		want[filepath.Join(ns, "p-c2.log")] = log + "container=c2"
		want[filepath.Join(ns, "q-e.log")] = "log of /api/v1/namespaces/" + ns + "/pods/q/log?container=e"
	}
	requireOnlyFiles(t, filepath.Join(dir, "logs"), want)
	if err == nil || strings.Count(err.Error(), "(get pods q)") != len(managers) {
		t.Errorf("the dump's error does not name the failed log of the Pod q once in each of the %d namespaces: %v", len(managers), err)
	}
}

func TestTheDumpGivesTheListsAndTheDiscoveryOfTheManagementClusterTheRequestTimeout(t *testing.T) {
	requests := requestsOfTheDump(t)
	for _, suffix := range []string{"/api", "/apis", "/clusters", "/events"} {
		requireRequestTimeout(t, requests, suffix, dumpRequestTimeout)
	}
}
