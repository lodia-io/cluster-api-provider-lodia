// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package clusterctlproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	TrapTheDefaultKubeconfig(t)
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

func TestNoCallReadsAKubeconfigOrTheCurrentContext(t *testing.T) {
	for call, run := range calls {
		t.Run(call, func(t *testing.T) {
			s := serve(t, neverAnswers)
			c := newClusterctl(t, s)
			ctx, cancel := context.WithTimeout(t.Context(), bound)
			defer cancel()
			if _, err := within(t, 2*request, call, func() error { return run(ctx, c) }); err == nil {
				t.Fatalf("%s says nothing about a cluster that did not answer", call)
			}
			if s.asked() == 0 {
				t.Errorf("%s did not reach the cluster of the rest config", call)
			}
		})
	}
}

func TestEveryClusterClientTheFactoryMakesReachesTheClusterOfTheRestConfig(t *testing.T) {
	TrapTheDefaultKubeconfig(t)
	s := serve(t, neverAnswers)
	configClient, err := config.New(t.Context(), "", config.InjectReader(config.NewMemoryReader()))
	if err != nil {
		t.Fatal(err)
	}
	factory := clusterClientFactory(configClient, &proxy{cfg: restConfig(s), requestTimeout: request, discoveryTimeout: discovery})
	for name, kubeconfig := range map[string]clusterctl.Kubeconfig{"the end that names none": {}, "the end that names the proxy's": Kubeconfig} {
		t.Run(name, func(t *testing.T) {
			c, err := factory(clusterctl.ClusterClientFactoryInput{Kubeconfig: kubeconfig})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Proxy().GetConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got.Host != s.url || got.Timeout != request {
				t.Errorf("the client reaches %s with the request timeout %s, want %s and %s", got.Host, got.Timeout, s.url, request)
			}
		})
	}
}

func TestAKubeconfigThatTheProxyWouldIgnoreIsRefused(t *testing.T) {
	for name, kubeconfig := range map[string]clusterctl.Kubeconfig{
		"a path":                            {Path: "/somewhere/kubeconfig"},
		"a context":                         {Context: "another"},
		"both":                              {Path: "/somewhere/kubeconfig", Context: "another"},
		"a path beside the proxy's context": {Path: "/somewhere/kubeconfig", Context: Kubeconfig.Context},
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

const (
	capiClusterctl    = "sigs.k8s.io/cluster-api/cmd/clusterctl"
	clusterctlClient  = capiClusterctl + "/client"
	clusterctlCluster = clusterctlClient + "/cluster"
	capiTestE2E       = "sigs.k8s.io/cluster-api/test/e2e"
	capiTestFramework = "sigs.k8s.io/cluster-api/test/framework"
	moduleRoot        = "../.."
)

// builtOutsideTheFactory lists the calls and imports of a file that build a
// clusterctl client in a way New does not. A call such as cluster.GetOwnerGraph
// makes clusterctl's own proxy from the kubeconfig path it is given, which loads
// KUBECONFIG and the current context when the path is empty:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/ownergraph.go#L71-L72
func builtOutsideTheFactory(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var found []string
	names := make(map[string]string)
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if importPath != clusterctlClient && importPath != clusterctlCluster {
			continue
		}
		switch {
		case imp.Name == nil:
			names[filepath.Base(importPath)] = importPath
		case imp.Name.Name == ".":
			found = append(found, fmt.Sprintf("%s imports %s with a dot, which hides its calls from the scan", fset.Position(imp.Pos()), importPath))
		default:
			names[imp.Name.Name] = importPath
		}
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
		x, ok := selector.X.(*ast.Ident)
		if !ok || names[x.Name] == "" {
			return true
		}
		if names[x.Name] == clusterctlCluster && (selector.Sel.Name == "InjectProxy" || selector.Sel.Name == "InjectYamlProcessor") {
			return true
		}
		found = append(found, fmt.Sprintf("%s calls %s.%s", fset.Position(call.Pos()), x.Name, selector.Sel.Name))
		return true
	})
	return found, nil
}

func TestTheScanRefusesEveryClusterctlClientNotBuiltByTheFactory(t *testing.T) {
	const (
		clusterImport = `import "` + clusterctlCluster + `"` + "\n"
		clientImport  = `import clusterctl "` + clusterctlClient + `"` + "\n"
	)
	cases := []struct {
		name, src string
		want      int
	}{
		{"clusterctl's own client", "package p\n" + clientImport + "var _, _ = clusterctl.New(nil, \"\")\n", 1},
		{"a cluster client", "package p\n" + clusterImport + "var _ = cluster.New(cluster.Kubeconfig{}, nil)\n", 1},
		{"clusterctl's own proxy", "package p\n" + clusterImport + "var _ = cluster.NewProxy(cluster.Kubeconfig{Path: \"/x\"}, 0)\n", 1},
		{"the owner graph from a kubeconfig path", "package p\n" + clusterImport + "var _, _ = cluster.GetOwnerGraph(nil, \"capi-join\", \"/some/kubeconfig\", nil)\n", 1},
		{"the owner graph from an empty path", "package p\n" + clusterImport + "var _, _ = cluster.GetOwnerGraph(nil, \"capi-join\", \"\", nil)\n", 1},
		{"a call of a package imported under another name", "package p\n" + `import owners "` + clusterctlCluster + `"` + "\nvar _, _ = owners.GetOwnerGraph(nil, \"n\", \"\", nil)\n", 1},
		{"two calls", "package p\n" + clusterImport + "var _ = cluster.NewProxy(cluster.Kubeconfig{}, 0)\nvar _, _ = cluster.GetOwnerGraph(nil, \"n\", \"\", nil)\n", 2},
		{"a package imported with a dot", "package p\n" + `import . "` + clusterctlCluster + `"` + "\nvar _, _ = GetOwnerGraph(nil, \"n\", \"\", nil)\n", 1},
		{"the option that gives cluster.New its Proxy", "package p\n" + clusterImport + "var _ = cluster.InjectProxy(nil)\n", 0},
		{"the option that gives cluster.New its processor", "package p\n" + clusterImport + "var _ = cluster.InjectYamlProcessor(nil)\n", 0},
		{"an option of the client package", "package p\n" + clientImport + "var _ = clusterctl.InjectConfig(nil)\n", 1},
		{"a type and a literal of the client", "package p\n" + clientImport + "var _ clusterctl.Client\nvar _ = clusterctl.Kubeconfig{Context: \"c\"}\n", 0},
		{"a call on a client", "package p\n" + clientImport + "func f(c clusterctl.Client) { _, _ = c.Init(nil, clusterctl.InitOptions{}) }\n", 0},
		{"a package of that name that is not clusterctl's", "package p\n" + `import "example.invalid/cluster"` + "\nvar _ = cluster.New()\n", 0},
		{"clusterctl's configuration", "package p\n" + `import "` + clusterctlClient + `/config"` + "\nvar _, _ = config.New(nil, \"\")\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			found, err := builtOutsideTheFactory("p.go", c.src)
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != c.want {
				t.Errorf("the scan found %d, want %d: %v", len(found), c.want, found)
			}
		})
	}
}

type listedModule struct {
	Path, Dir string
	Main      bool
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Imports    []string
	Module     *listedModule
}

func (p listedPackage) inTheModule() bool { return p.Module != nil && p.Module.Main }

func under(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

// buildingClustersFromAPath lists the packages of a dependency graph outside
// this module that build clusterctl clients in a way New does not, under any
// import name: Cluster API's test/e2e and test/framework, whose clusterctl
// clients take the kubeconfig path of the bootstrap proxy, and every other
// package that imports clusterctl's client or client/cluster, clusterctl's own
// excepted.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/e2e/quick_start.go#L208-L214
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/framework/clusterctl/client.go#L323-L327
func buildingClustersFromAPath(pkgs []listedPackage) []string {
	var found []string
	for _, p := range pkgs {
		if p.inTheModule() {
			continue
		}
		path, _, _ := strings.Cut(p.ImportPath, " ")
		switch {
		case under(path, capiTestE2E) || under(path, capiTestFramework):
			found = append(found, fmt.Sprintf("%s is in the dependency graph, and its clusterctl clients take a kubeconfig path", path))
		case under(path, capiClusterctl):
		case slices.Contains(p.Imports, clusterctlClient) || slices.Contains(p.Imports, clusterctlCluster):
			found = append(found, fmt.Sprintf("%s imports clusterctl's client, and builds its clusters from a kubeconfig path", path))
		}
	}
	return found
}

func listPackages(t *testing.T, root, tags string) []listedPackage {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the root of the module: %v", root, err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "list", "-tags="+tags, "-deps", "-test", "-json=ImportPath,Dir,Imports,Module", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		t.Fatalf("go list: %v\n%s", err, exitErr.Stderr)
	} else if err != nil {
		t.Fatal(err)
	}
	var pkgs []listedPackage
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func TestTheGraphScanRefusesEveryPackageThatBuildsClusterctlClientsFromAPath(t *testing.T) {
	outside := func(path string, imports ...string) listedPackage {
		return listedPackage{ImportPath: path, Imports: imports}
	}
	inside := outside("go.lodia.io/m/test/e2e", clusterctlClient, clusterctlCluster)
	inside.Module = &listedModule{Path: "go.lodia.io/m", Dir: "/m", Main: true}
	cases := []struct {
		name string
		pkg  listedPackage
		want int
	}{
		{"Cluster API's test/e2e", outside(capiTestE2E), 1},
		{"Cluster API's test/e2e, built for a test", outside(capiTestE2E + " [" + capiTestE2E + ".test]"), 1},
		{"a package of Cluster API's test/e2e", outside(capiTestE2E + "/internal"), 1},
		{"the test framework", outside(capiTestFramework), 1},
		{"the test framework's clusterctl", outside(capiTestFramework+"/clusterctl", clusterctlClient), 1},
		{"a package beside the test framework", outside(capiTestFramework + "x"), 0},
		{"a package beside the test e2e", outside(capiTestE2E + "x"), 0},
		{"a package of another module that imports clusterctl's client", outside("example.invalid/p", clusterctlClient), 1},
		{"a package of another module that imports clusterctl's cluster client", outside("example.invalid/p", "io", clusterctlCluster), 1},
		{"a package of another module that imports clusterctl's configuration", outside("example.invalid/p", clusterctlClient+"/config"), 0},
		{"a package of another module that imports nothing of clusterctl", outside("example.invalid/p", "io"), 0},
		{"clusterctl's own package", outside(clusterctlClient+"/alpha", clusterctlCluster), 0},
		{"clusterctl's own client", outside(clusterctlClient, clusterctlCluster), 0},
		{"a package beside clusterctl", outside(capiClusterctl+"x/p", clusterctlClient), 1},
		{"a package of this module, whose files the other scan reads", inside, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if found := buildingClustersFromAPath([]listedPackage{c.pkg}); len(found) != c.want {
				t.Errorf("the scan found %d, want %d: %v", len(found), c.want, found)
			}
		})
	}
}

func TestTheListingHoldsTheImportsOfTestFilesAndOfTheTagsItIsGiven(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":    "module example.invalid/m\n\ngo 1.26\n",
		"a.go":      "package m\n",
		"a_test.go": "package m\n\nimport _ \"go/parser\"\n",
		"b.go":      "//go:build e2e\n\npackage m\n\nimport _ \"go/printer\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for tags, want := range map[string]map[string]bool{
		"":    {"go/parser": true, "go/printer": false},
		"e2e": {"go/parser": true, "go/printer": true},
	} {
		imports := make(map[string]bool)
		for _, p := range listPackages(t, dir, tags) {
			for _, imp := range p.Imports {
				imports[imp] = true
			}
		}
		for imp, imported := range want {
			if imports[imp] != imported {
				t.Errorf("with the tags %q the listing holds an import of %s: %t, want %t", tags, imp, imports[imp], imported)
			}
		}
	}
}

func TestNoPackageOfTheDependencyGraphBuildsClusterctlClientsFromAPath(t *testing.T) {
	for name, tags := range map[string]string{"untagged": "", "e2e": "e2e"} {
		t.Run(name, func(t *testing.T) {
			for _, f := range buildingClustersFromAPath(listPackages(t, moduleRoot, tags)) {
				t.Error(f)
			}
		})
	}
}

func notScanned(pkgs []listedPackage, scanned map[string]bool) []string {
	var missing []string
	for _, p := range pkgs {
		if !p.inTheModule() {
			continue
		}
		dir, err := filepath.Rel(p.Module.Dir, p.Dir)
		if err != nil {
			dir = p.Dir
		}
		if dir = filepath.ToSlash(dir); !scanned[dir] && !slices.Contains(missing, dir) {
			missing = append(missing, dir)
		}
	}
	slices.Sort(missing)
	return missing
}

func TestTheWalkReadsEveryPackageOfTheModule(t *testing.T) {
	module := &listedModule{Path: "go.lodia.io/m", Dir: "/m", Main: true}
	pkgs := []listedPackage{
		{ImportPath: "go.lodia.io/m/internal/controller", Dir: "/m/internal/controller", Module: module},
		{ImportPath: "go.lodia.io/m/internal/controller_test [go.lodia.io/m/internal/controller.test]", Dir: "/m/internal/controller", Module: module},
		{ImportPath: "go.lodia.io/m/test/e2e", Dir: "/m/test/e2e", Module: module},
		{ImportPath: "example.invalid/p", Dir: "/elsewhere/p"},
	}
	for name, c := range map[string]struct {
		scanned map[string]bool
		want    []string
	}{
		"every directory":           {map[string]bool{"internal/controller": true, "test/e2e": true}, nil},
		"a walk that began in test": {map[string]bool{"test/e2e": true}, []string{"internal/controller"}},
		"a walk that read nothing":  {nil, []string{"internal/controller", "test/e2e"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := notScanned(pkgs, c.scanned); !slices.Equal(got, c.want) {
				t.Errorf("the directories not scanned are %v, want %v", got, c.want)
			}
		})
	}
}

// Every clusterctl client the suite builds has to go through New, or it reads
// the kubeconfig that clusterctl's own proxy loads and has no bound.
func TestEveryClusterctlClientIsBuiltThroughTheFactory(t *testing.T) {
	allowed, err := filepath.Abs("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	scanned := make(map[string]bool)
	err = filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
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
		found, err := builtOutsideTheFactory(path, nil)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(abs))
		if err != nil {
			return err
		}
		scanned[filepath.ToSlash(dir)] = true
		for _, f := range found {
			t.Errorf("%s: clusterctl's clients are built by clusterctlproxy.New alone", f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if missing := notScanned(listPackages(t, moduleRoot, "e2e"), scanned); len(missing) > 0 {
		t.Fatalf("the scan read no Go file in the directories %v, which hold packages of the module", missing)
	}
}
