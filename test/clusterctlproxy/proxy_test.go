// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package clusterctlproxy

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1beta1 "sigs.k8s.io/cluster-api/api/core/v1beta1" //nolint:staticcheck // clusterctl's own scheme holds the v1beta1 kinds
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

const (
	request   = 5 * time.Second
	discovery = 500 * time.Millisecond
	bound     = time.Second
	slack     = time.Second
)

type server struct {
	url  string
	mu   sync.Mutex
	hits int
}

func (s *server) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func serve(t *testing.T, answers func(path string) (body string, ok bool)) *server {
	t.Helper()
	s := &server{}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		body, ok := answers(r.URL.Path)
		if !ok {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	s.url = ts.URL
	return s
}

func neverAnswers(string) (string, bool) { return "", false }

func answersTheVersionOnly(path string) (string, bool) {
	return `{"major":"1","minor":"35","gitVersion":"v1.35.0"}`, path == "/version"
}

func neverCompletesTLS(t *testing.T) *server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{url: "https://" + ln.Addr().String()}
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.hits++
			conns = append(conns, conn)
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return s
}

func restConfig(s *server) *rest.Config {
	return &rest.Config{Host: s.url, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
}

func newClusterctl(t *testing.T, s *server) clusterctl.Client {
	t.Helper()
	configClient, err := config.New(t.Context(), "", config.InjectReader(config.NewMemoryReader()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(t.Context(), configClient, restConfig(s), request, discovery)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var calls = map[string]func(context.Context, clusterctl.Client) error{
	"Init": func(ctx context.Context, c clusterctl.Client) error {
		_, err := c.Init(ctx, clusterctl.InitOptions{CoreProvider: "cluster-api", ControlPlaneProviders: []string{"lodia"}})
		return err
	},
	"Move": func(ctx context.Context, c clusterctl.Client) error {
		return c.Move(ctx, clusterctl.MoveOptions{ToKubeconfig: Kubeconfig, Namespace: "capi-join"})
	},
}

func within(t *testing.T, limit time.Duration, what string, call func() error) (time.Duration, error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- call() }()
	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(limit):
		t.Fatalf("%s is still running after %s", what, limit)
		return 0, nil
	}
}

func TestInitAndMoveAgainstAClusterThatDoesNotAnswerEndWithinTheirBoundAndOneDiscoveryTimeout(t *testing.T) {
	servers := map[string]func(*testing.T) *server{
		"a cluster that accepts connections and never completes TLS": neverCompletesTLS,
		"a cluster that completes TLS and never answers":             func(t *testing.T) *server { return serve(t, neverAnswers) },
		"a cluster that answers the version and never discovery":     func(t *testing.T) *server { return serve(t, answersTheVersionOnly) },
	}
	for name, start := range servers {
		for call, run := range calls {
			t.Run(name+"/"+call, func(t *testing.T) {
				s := start(t)
				c := newClusterctl(t, s)
				ctx, cancel := context.WithTimeout(t.Context(), bound)
				defer cancel()
				took, err := within(t, 2*request, call, func() error { return run(ctx, c) })
				if err == nil {
					t.Fatalf("%s says nothing about a cluster that did not answer", call)
				}
				if took > bound+discovery+slack {
					t.Errorf("%s ended after %s, more than its bound %s and a discovery timeout %s (and %s of slack) after it, with the request timeout at %s: %v", call, took, bound, discovery, slack, request, err)
				}
				if s.asked() == 0 {
					t.Errorf("%s asked the cluster nothing: %v", call, err)
				}
			})
		}
	}
}

func writeKubeconfig(t *testing.T, path, server string) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["trap"] = &clientcmdapi.Cluster{Server: server, InsecureSkipTLSVerify: true}
	cfg.AuthInfos["trap"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["trap"] = &clientcmdapi.Context{Cluster: "trap", AuthInfo: "trap"}
	cfg.CurrentContext = "trap"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
}

func TestNoCallReadsAKubeconfigOrTheCurrentContext(t *testing.T) {
	trap := serve(t, neverAnswers)
	home := t.TempDir()
	writeKubeconfig(t, filepath.Join(home, ".kube", "config"), trap.url)
	writeKubeconfig(t, filepath.Join(home, "elsewhere"), trap.url)
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "elsewhere"))
	for call, run := range calls {
		t.Run(call, func(t *testing.T) {
			s := serve(t, neverAnswers)
			ctx, cancel := context.WithTimeout(t.Context(), bound)
			defer cancel()
			if _, err := within(t, 2*request, call, func() error { return run(ctx, newClusterctl(t, s)) }); err == nil {
				t.Fatalf("%s says nothing about a cluster that did not answer", call)
			}
			if s.asked() == 0 {
				t.Errorf("%s did not reach the cluster of the rest config", call)
			}
			if n := trap.asked(); n > 0 {
				t.Errorf("%s asked the cluster of the default kubeconfig %d times", call, n)
			}
		})
	}
}

func TestAKubeconfigThatTheProxyWouldIgnoreIsRefused(t *testing.T) {
	for name, kubeconfig := range map[string]clusterctl.Kubeconfig{
		"a path":    {Path: "/somewhere/kubeconfig"},
		"a context": {Context: "another"},
		"both":      {Path: "/somewhere/kubeconfig", Context: "another"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := requireNoKubeconfig(kubeconfig); err == nil || !strings.Contains(err.Error(), "which the suite's proxy would ignore") {
				t.Fatalf("want the refusal of the kubeconfig, got %v", err)
			}
			for call, run := range map[string]func(clusterctl.Client) error{
				"Init": func(c clusterctl.Client) error {
					_, err := c.Init(t.Context(), clusterctl.InitOptions{Kubeconfig: kubeconfig, CoreProvider: "cluster-api"})
					return err
				},
				"Move from": func(c clusterctl.Client) error {
					return c.Move(t.Context(), clusterctl.MoveOptions{FromKubeconfig: kubeconfig, ToKubeconfig: Kubeconfig, Namespace: "capi-join"})
				},
			} {
				s := serve(t, neverAnswers)
				if err := run(newClusterctl(t, s)); err == nil || !strings.Contains(err.Error(), "which the suite's proxy would ignore") {
					t.Errorf("%s: want the refusal of the kubeconfig, got %v", call, err)
				}
				if n := s.asked(); n > 0 {
					t.Errorf("%s: the cluster was asked %d times before the refusal", call, n)
				}
			}
		})
	}
	for name, kubeconfig := range map[string]clusterctl.Kubeconfig{"none": {}, "the proxy's": Kubeconfig} {
		if err := requireNoKubeconfig(kubeconfig); err != nil {
			t.Errorf("%s: want accepted, got %v", name, err)
		}
	}
}

func TestTheSchemeHoldsTheKindsOfClusterctlsOwn(t *testing.T) {
	for _, gvk := range []schema.GroupVersionKind{
		appsv1.SchemeGroupVersion.WithKind("Deployment"),
		clusterctlv1.GroupVersion.WithKind("Provider"),
		clusterv1beta1.GroupVersion.WithKind("Cluster"),
		clusterv1.GroupVersion.WithKind("Cluster"),
		apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"),
		apiextensionsv1beta1.SchemeGroupVersion.WithKind("CustomResourceDefinition"),
		admissionregistrationv1.SchemeGroupVersion.WithKind("ValidatingWebhookConfiguration"),
		admissionregistrationv1beta1.SchemeGroupVersion.WithKind("MutatingWebhookConfiguration"),
		controlplanev1.GroupVersion.WithKind("KubeadmControlPlane"),
		addonsv1.GroupVersion.WithKind("ClusterResourceSet"),
	} {
		if !scheme.Recognizes(gvk) {
			t.Errorf("the scheme does not recognize %s", gvk)
		}
	}
}

// Every clusterctl client the suite builds has to go through New, or it reads
// the kubeconfig that clusterctl's own proxy loads and has no bound.
func TestEveryClusterctlClientIsBuiltThroughTheFactory(t *testing.T) {
	const clusterctlClient = "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	builders := map[string]bool{clusterctlClient: true, clusterctlClient + "/cluster": true}
	allowed, err := filepath.Abs("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	err = filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "work" || name == "out" {
				return filepath.SkipDir
			}
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil || !strings.HasSuffix(path, ".go") || abs == allowed {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		names := make(map[string]bool)
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !builders[importPath] {
				continue
			}
			name := filepath.Base(importPath)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			names[name] = true
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := selector.X.(*ast.Ident); ok && names[x.Name] && (selector.Sel.Name == "New" || selector.Sel.Name == "NewProxy") {
				t.Errorf("%s calls %s.%s: clusterctl's clients are built by clusterctlproxy.New alone", path, x.Name, selector.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 10 {
		t.Fatalf("the scan read %d Go files, fewer than the module holds", scanned)
	}
}
