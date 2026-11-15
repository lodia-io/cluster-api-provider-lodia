// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func requireNoRateLimiter(t *testing.T, cfg *rest.Config) {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if limiter := clientset.CoreV1().RESTClient().GetRateLimiter(); limiter != nil {
		t.Errorf("a client of the config has the rate limiter %T", limiter)
	}
}

func TestAConfigThatSetsNoQPSKeepsClientGosRateLimiter(t *testing.T) {
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: "https://unused.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if clientset.CoreV1().RESTClient().GetRateLimiter() == nil {
		t.Fatal("a client of a config with no QPS has no rate limiter, so the checks that none is there see nothing")
	}
}

func TestTheConfigOfTheDumpHasNoRateLimiter(t *testing.T) {
	requireNoRateLimiter(t, dumpRESTConfig(configFor(t, "https://unused.invalid")))
}

func TestTheConfigOfATenantHasNoRateLimiter(t *testing.T) {
	key := client.ObjectKey{Namespace: namespace, Name: "c-w"}
	k8s := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)},
		Data:       map[string][]byte{secret.KubeconfigDataName: serverKubeconfig(t, "https://unused.invalid", "tenant")},
	}).Build()
	tenant, err := newTenantAccess(t.Context(), k8s, key, tenantRequestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	requireNoRateLimiter(t, tenant.cfg)
}

var (
	sourcesOfAConfig = []string{"clientcmd.RESTConfigFromKubeConfig", "clientcmd.BuildConfigFromFlags", "rest.InClusterConfig", "rest.CopyConfig"}
	takeAConfig      = []string{"clusterctlproxy.New", "clusterctlproxy.NewClient"}
)

func isNoRateLimit(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "NoRateLimit"
	case *ast.SelectorExpr:
		return e.Sel.Name == "NoRateLimit"
	case *ast.UnaryExpr:
		lit, ok := e.X.(*ast.BasicLit)
		return ok && e.Op == token.SUB && lit.Value == "1"
	}
	return false
}

func setsNoRateLimit(fn *ast.FuncDecl) bool {
	var found bool
	for n := range ast.Preorder(fn.Body) {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for i, lhs := range assign.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "QPS" && len(assign.Rhs) == len(assign.Lhs) && isNoRateLimit(assign.Rhs[i]) {
				found = true
			}
		}
	}
	return found
}

func isARestConfig(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	callee := calleeOf(call)
	if callee == "rest.CopyConfig" {
		sel, ok := argumentOf(call, 0).(*ast.SelectorExpr)
		return !ok || types.ExprString(sel) != "p.cfg"
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RESTConfig" && len(call.Args) == 0 {
		return true
	}
	return slices.Contains(sourcesOfAConfig, callee)
}

func problemsWithTheRateLimiters(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	for _, file := range files {
		ast.PreorderStack(file, nil, func(n ast.Node, stack []ast.Node) bool {
			if !isARestConfig(n) {
				return true
			}
			if len(stack) > 0 {
				if call, ok := stack[len(stack)-1].(*ast.CallExpr); ok && slices.Contains(takeAConfig, calleeOf(call)) {
					return true
				}
			}
			var fn *ast.FuncDecl
			for _, outer := range slices.Backward(stack) {
				if decl, ok := outer.(*ast.FuncDecl); ok {
					fn = decl
					break
				}
			}
			if fn == nil || !setsNoRateLimit(fn) {
				problems = append(problems, problemAt(fset, n, "a rest.Config that keeps client-go's rate limiter, which refuses a request under a deadline with an error that is no context's: set its QPS to clusterctlproxy.NoRateLimit, or hand it to clusterctlproxy.New or clusterctlproxy.NewClient"))
			}
			return true
		})
	}
	return problems
}

func TestNoRESTConfigOfTheSuiteKeepsClientGosRateLimiter(t *testing.T) {
	var names []string
	for _, pattern := range []string{"*.go", filepath.Join("..", "clusterctlproxy", "*.go")} {
		matched, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, slices.DeleteFunc(matched, func(name string) bool {
			return strings.HasPrefix(pattern, "..") && strings.HasSuffix(name, "_test.go")
		})...)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) < 2 {
		t.Fatalf("the check read %d files", len(files))
	}
	for _, problem := range problemsWithTheRateLimiters(fset, files) {
		t.Error(problem)
	}
}

func TestTheCheckOfTheRESTConfigsRefusesOneThatKeepsTheRateLimiter(t *testing.T) {
	const refused = "keeps client-go's rate limiter"
	cases := []struct {
		name, source, want string
	}{
		{"a copy in a function that sets QPS", "func helper() {\ncfg := rest.CopyConfig(other)\ncfg.QPS = clusterctlproxy.NoRateLimit\n}", ""},
		{"a copy whose QPS is set to -1", "func helper() {\ncfg := rest.CopyConfig(other)\ncfg.QPS = -1\n}", ""},
		{"a copy of the proxy's own config", "func helper() {\ncfg := rest.CopyConfig(p.cfg)\ncfg.Timeout = 1\n}", ""},
		{"a config of a kubeconfig whose QPS is set", "func helper() {\ncfg, err := clientcmd.RESTConfigFromKubeConfig(raw)\ncfg.QPS = clusterctlproxy.NoRateLimit\n}", ""},
		{"a config handed to clusterctlproxy.NewClient", "func helper() {\nclusterctlproxy.NewClient(cfg.Client().RESTConfig(), scheme, a, b)\n}", ""},
		{"a copy whose QPS is not set", "func helper() {\ncfg := rest.CopyConfig(other)\ncfg.Timeout = 1\n}", refused},
		{"a copy whose QPS is set to something else", "func helper() {\ncfg := rest.CopyConfig(other)\ncfg.QPS = 500\n}", refused},
		{"a copy whose QPS is read and not set", "func helper() {\ncfg := rest.CopyConfig(other)\n_ = cfg.QPS\n}", refused},
		{"a copy in a function that has no QPS but another function has", "func helper() {\ncfg := rest.CopyConfig(other)\n}\nfunc another() {\ncfg.QPS = -1\n}", refused},
		{"a copy outside a function", "var cfg = rest.CopyConfig(other)", refused},
		{"a config of a kubeconfig whose QPS is not set", "func helper() {\ncfg, err := clientcmd.RESTConfigFromKubeConfig(raw)\n}", refused},
		{"a config of a kubeconfig file", "func helper() {\ncfg, err := clientcmd.BuildConfigFromFlags(\"\", path)\n}", refused},
		{"the in-cluster config", "func helper() {\ncfg, err := rest.InClusterConfig()\n}", refused},
		{"the config of the environment's client, kept", "func helper() {\ncfg := env.Client().RESTConfig()\n}", refused},
		{"the config of the environment's client, handed to another function", "func helper() {\nuse(env.Client().RESTConfig())\n}", refused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package p\n"+c.source+"\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheRateLimiters(fset, []*ast.File{file}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}
