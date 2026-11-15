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

func TestTheClientOfTheEnvironmentHasNoRateLimiter(t *testing.T) {
	requireNoRateLimiter(t, configFor(t, "https://unused.invalid").Client().Resources().GetConfig())
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
	sourcesOfAConfig = []string{"clientcmd.RESTConfigFromKubeConfig", "clientcmd.BuildConfigFromFlags", "conf.New", "conf.NewWithContextName", "conf.NewInCluster", "rest.InClusterConfig", "rest.CopyConfig"}
	takeAConfig      = []string{"clusterctlproxy.New", "clusterctlproxy.NewClient"}
	makeAnEnvconf    = []string{"envconf.New", "envconf.NewWithKubeConfig", "envconf.NewFromFlags", "env.New", "env.NewParallel", "env.NewWithKubeConfig", "env.NewFromFlags", "env.NewInClusterConfig"}
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

func isARestConfigLiteral(n ast.Node) bool {
	lit, ok := n.(*ast.CompositeLit)
	if !ok {
		return false
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	return ok && types.ExprString(sel) == "rest.Config"
}

func isAnEnvconf(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	return ok && slices.Contains(makeAnEnvconf, calleeOf(call))
}

func isAClientOfAKubeconfigFile(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	return ok && calleeOf(call) == "klient.NewWithKubeConfigFile"
}

func problemsWithTheRateLimiters(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	for _, file := range files {
		ast.PreorderStack(file, nil, func(n ast.Node, stack []ast.Node) bool {
			var fn *ast.FuncDecl
			for _, outer := range slices.Backward(stack) {
				if decl, ok := outer.(*ast.FuncDecl); ok {
					fn = decl
					break
				}
			}
			if isARestConfigLiteral(n) && (carriesABuildTag(file) || fn == nil || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test")) {
				problems = append(problems, problemAt(fset, n, "a rest.Config literal outside a Test function of a file with no build tag, which keeps client-go's rate limiter unless it sets its QPS to clusterctlproxy.NoRateLimit: write it in a test of a fake, or take the config from a constructor that sets it"))
			}
			if isAnEnvconf(n) && (fn == nil || fn.Name.Name != "newEnvconf") {
				problems = append(problems, problemAt(fset, n, "an envconf, or an environment that makes one, whose client keeps client-go's rate limiter, since envconf's Client builds one with the default QPS: make the envconf with newEnvconf and the environment with env.NewWithConfig"))
			}
			if isAClientOfAKubeconfigFile(n) {
				problems = append(problems, problemAt(fset, n, "a client of a kubeconfig file, which keeps client-go's rate limiter since nothing sets the QPS of its config: make the config with conf.New, set its QPS to clusterctlproxy.NoRateLimit and hand it to klient.New, as newEnvconf does"))
			}
			if !isARestConfig(n) {
				return true
			}
			if len(stack) > 0 {
				if call, ok := stack[len(stack)-1].(*ast.CallExpr); ok && slices.Contains(takeAConfig, calleeOf(call)) {
					return true
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
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) < 2 {
		t.Fatalf("the check read %d files", len(files))
	}
	if !slices.ContainsFunc(files, carriesABuildTag) {
		t.Fatal("none of the files the check read carries a build tag, so it cannot refuse a rest.Config literal in one")
	}
	for _, problem := range problemsWithTheRateLimiters(fset, files) {
		t.Error(problem)
	}
}

func requireProblemOfSource(t *testing.T, src, want string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	problems := strings.Join(problemsWithTheRateLimiters(fset, []*ast.File{file}), "\n")
	switch {
	case want == "" && problems != "":
		t.Fatalf("want accepted, got %s", problems)
	case want != "" && !strings.Contains(problems, want):
		t.Fatalf("want a problem with %q, got %q", want, problems)
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
		{"a config of conf.New whose QPS is set", "func newEnvconf() {\ncfg, err := conf.New(path)\ncfg.QPS = clusterctlproxy.NoRateLimit\n}", ""},
		{"a config of conf.New whose QPS is not set", "func helper() {\ncfg, err := conf.New(path)\n}", refused},
		{"a config of conf.NewWithContextName whose QPS is set", "func helper() {\ncfg, err := conf.NewWithContextName(path, \"c\")\ncfg.QPS = clusterctlproxy.NoRateLimit\n}", ""},
		{"a config of conf.NewWithContextName whose QPS is not set", "func helper() {\ncfg, err := conf.NewWithContextName(path, \"c\")\n}", refused},
		{"a config of conf.NewInCluster whose QPS is set", "func helper() {\ncfg, err := conf.NewInCluster()\ncfg.QPS = clusterctlproxy.NoRateLimit\n}", ""},
		{"a config of conf.NewInCluster whose QPS is not set", "func helper() {\ncfg, err := conf.NewInCluster()\n}", refused},
		{"a client of a config whose QPS is set", "func helper() {\ncfg, err := conf.New(path)\ncfg.QPS = clusterctlproxy.NoRateLimit\nuse(klient.New(cfg))\n}", ""},
		{"a client of a kubeconfig file", "func helper() {\nuse(klient.NewWithKubeConfigFile(path))\n}", refused},
		{"a client of a kubeconfig file in newEnvconf", "func newEnvconf(path string) {\nuse(klient.NewWithKubeConfigFile(path))\n}", refused},
		{"a client of a kubeconfig file outside a function", "var c, _ = klient.NewWithKubeConfigFile(path)", refused},
		{"a rest.Config literal in a Test function", "func TestX(t *testing.T) {\nuse(&rest.Config{Host: \"https://unused.invalid\"})\n}", ""},
		{"a rest.Config literal in a function literal of a Test function", "func TestX(t *testing.T) {\nt.Run(\"a\", func(t *testing.T) {\nuse(rest.Config{})\n})\n}", ""},
		{"a rest.Config literal in a helper", "func helper() {\nuse(&rest.Config{Host: \"https://unused.invalid\"})\n}", refused},
		{"a rest.Config literal in a method named like a Test", "func (s suite) TestX() {\nuse(&rest.Config{})\n}", refused},
		{"a rest.Config literal outside a function", "var cfg = &rest.Config{}", refused},
		{"an envconf made by newEnvconf", "func newEnvconf(path string) {\nuse(envconf.NewWithKubeConfig(path))\n}", ""},
		{"an envconf of NewWithKubeConfig outside newEnvconf", "func TestMain(m *testing.M) {\ntestenv = env.NewWithConfig(envconf.NewWithKubeConfig(path))\n}", refused},
		{"an envconf of New outside newEnvconf", "func helper() {\nuse(envconf.New())\n}", refused},
		{"an envconf of NewFromFlags outside newEnvconf", "func helper() {\nuse(envconf.NewFromFlags())\n}", refused},
		{"an envconf outside a function", "var cfg = envconf.New()", refused},
		{"an environment of an envconf that newEnvconf made", "func TestMain(m *testing.M) {\ntestenv = env.NewWithConfig(cfg)\n}", ""},
		{"an environment of New", "func TestMain(m *testing.M) {\ntestenv = env.New()\n}", refused},
		{"an environment of NewParallel", "func TestMain(m *testing.M) {\ntestenv = env.NewParallel()\n}", refused},
		{"an environment of NewWithKubeConfig", "func TestMain(m *testing.M) {\ntestenv = env.NewWithKubeConfig(path)\n}", refused},
		{"an environment of NewFromFlags", "func TestMain(m *testing.M) {\ntestenv, err = env.NewFromFlags()\n}", refused},
		{"an environment of NewInClusterConfig", "func TestMain(m *testing.M) {\ntestenv = env.NewInClusterConfig()\n}", refused},
		{"an environment outside a function", "var testenv = env.New()", refused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireProblemOfSource(t, "package p\n"+c.source+"\n", c.want)
		})
	}
}

func TestTheCheckOfTheRESTConfigsDecidesTheLiteralByTheFile(t *testing.T) {
	const refused = "keeps client-go's rate limiter"
	cases := []struct {
		name, source, want string
	}{
		{"a literal in a Test function of a file with no build tag", "// Copyright\n\npackage p\n\nfunc TestX(t *testing.T) {\nuse(&rest.Config{})\n}", ""},
		{"a literal in a Test function of a file with a build tag", "//go:build e2e\n\npackage p\n\nfunc TestX(t *testing.T) {\nuse(&rest.Config{})\n}", refused},
		{"a literal in a function literal of a Test function of a file with a build tag", "//go:build e2e && upgrade\n\npackage p\n\nfunc TestMain(m *testing.M) {\nf := features.New(\"a\").Assess(\"b\", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {\nuse(rest.Config{})\nreturn ctx\n})\n}", refused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireProblemOfSource(t, c.source+"\n", c.want)
		})
	}
}
