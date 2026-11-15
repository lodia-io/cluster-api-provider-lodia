// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func identNamed(e ast.Expr, name string) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == name
}

func contextParameterOf(fn *ast.FuncDecl) string {
	for _, field := range fn.Type.Params.List {
		if kind, ok := field.Type.(*ast.SelectorExpr); ok && identNamed(kind.X, "context") && kind.Sel.Name == "Context" && len(field.Names) > 0 {
			return field.Names[0].Name
		}
	}
	return ""
}

func isAContextMaker(callee string) bool {
	return callee == "within" || (strings.HasPrefix(callee, "context.") && callee != "context.Cause")
}

func repeats(n ast.Node) bool {
	switch n.(type) {
	case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
		return true
	}
	return false
}

func problemAt(fset *token.FileSet, n ast.Node, format string, args ...any) string {
	return fmt.Sprintf("%s: %s", fset.Position(n.Pos()), fmt.Sprintf(format, args...))
}

func problemsWithTheInstall(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	installs := make(map[string]ast.Node)
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || calleeOf(call) != "testenv.Setup" {
				return true
			}
			for _, arg := range call.Args {
				if ident, ok := arg.(*ast.Ident); ok {
					installs[ident.Name] = call
				} else {
					problems = append(problems, problemAt(fset, arg, "testenv.Setup is handed something but the name of a function, which this check does not read"))
				}
			}
			return true
		})
	}
	if len(installs) == 0 {
		problems = append(problems, "no file hands a function to testenv.Setup, so there is no install to check")
	}
	found := make(map[string]bool)
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && installs[fn.Name.Name] != nil && fn.Body != nil {
				found[fn.Name.Name] = true
				problems = append(problems, problemsWithTheContextOfTheInstall(fset, fn)...)
			}
		}
	}
	for name, setup := range installs {
		if !found[name] {
			problems = append(problems, problemAt(fset, setup, "testenv.Setup is handed %s, which the files declare no function of", name))
		}
	}
	return problems
}

func problemsWithTheContextOfTheInstall(fset *token.FileSet, fn *ast.FuncDecl) []string {
	var problems []string
	suite := contextParameterOf(fn)
	var bounded string
	var waits int
	ast.PreorderStack(fn.Body, nil, func(n ast.Node, stack []ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if bounded != "" && slices.ContainsFunc(n.Lhs, func(lhs ast.Expr) bool { return identNamed(lhs, bounded) }) {
				problems = append(problems, problemAt(fset, n, "%s is assigned again, so a wait of %s may run on a context that no bound ends", bounded, fn.Name.Name))
			}
		case *ast.CallExpr:
			callee := calleeOf(n)
			switch {
			case isAContextMaker(callee):
				assigned, _ := stack[len(stack)-1].(*ast.AssignStmt)
				field, isField := argumentOf(n, 1).(*ast.SelectorExpr)
				var made *ast.Ident
				if assigned != nil {
					made, _ = assigned.Lhs[0].(*ast.Ident)
				}
				if callee != "within" || !identNamed(argumentOf(n, 0), suite) || !isField || !isBoundsField(field, "suiteBounds") || field.Sel.Name != "Install" || made == nil || bounded != "" || slices.ContainsFunc(stack, repeats) {
					problems = append(problems, problemAt(fset, n, "%s makes a context of %s, whose one context has to be made by within(%s, suiteBounds.Install)", callee, fn.Name.Name, suite))
					break
				}
				bounded = made.Name
			case identNamed(argumentOf(n, 0), suite) && callee != "dump":
				problems = append(problems, problemAt(fset, n, "%s is given the suite's own context, which no bound ends, in %s", callee, fn.Name.Name))
			case bounded != "" && identNamed(argumentOf(n, 0), bounded) && callee != "context.Cause":
				waits++
			}
		}
		return true
	})
	switch {
	case bounded == "":
		problems = append(problems, problemAt(fset, fn, "%s makes no context with within(%s, suiteBounds.Install) for its waits to run on", fn.Name.Name, suite))
	case waits == 0:
		problems = append(problems, problemAt(fset, fn, "no wait of %s runs on the context that within(%s, suiteBounds.Install) made", fn.Name.Name, suite))
	}
	return problems
}

func problemsWithRootContexts(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	for _, file := range files {
		for n := range ast.Preorder(file) {
			if call, ok := n.(*ast.CallExpr); ok && slices.Contains([]string{"context.Background", "context.TODO", "context.WithoutCancel"}, calleeOf(call)) {
				problems = append(problems, problemAt(fset, call, "%s makes a context that no bound of the suite ends, in a file where every context comes from the one that the step or the install is given", calleeOf(call)))
			}
		}
	}
	return problems
}

func carriesABuildTag(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			return false
		}
		for _, c := range group.List {
			if strings.HasPrefix(c.Text, "//go:build ") {
				return true
			}
		}
	}
	return false
}

func taggedFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var tagged []*ast.File
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if carriesABuildTag(file) {
			tagged = append(tagged, file)
		}
	}
	if len(tagged) == 0 {
		t.Fatal("no file of the package carries a build tag")
	}
	return fset, tagged
}

func TestTheInstallRunsItsWaitsOnAContextThatEndsAtTheBoundOfTheInstall(t *testing.T) {
	fset, files := taggedFiles(t)
	for _, problem := range problemsWithTheInstall(fset, files) {
		t.Error(problem)
	}
}

func TestNoFileWithABuildTagMakesAContextThatNoBoundEnds(t *testing.T) {
	fset, files := taggedFiles(t)
	for _, problem := range problemsWithRootContexts(fset, files) {
		t.Error(problem)
	}
}

const theInstall = `bounded, cancel := within(ctx, suiteBounds.Install)
defer cancel()
for _, component := range components {
	start := time.Now()
	if err := component.install(bounded, cfg); err != nil {
		dump(ctx, cfg)
		return ctx, fmt.Errorf("install %s: %w", component.name, errors.Join(err, context.Cause(bounded)))
	}
	fmt.Printf("installed %s after %s\n", component.name, time.Since(start).Round(time.Second))
}
return ctx, nil`

func TestTheCheckOfTheInstallRefusesAWaitOnAContextThatNoBoundOfTheInstallEnds(t *testing.T) {
	const setup = "install"
	cases := []struct {
		name, setup, old, new, want string
	}{
		{"the install as it is", setup, "return ctx, nil", "return ctx, nil", ""},
		{"a second wait on the bounded context", setup, "fmt.Printf", "component.verify(bounded, cfg)\nfmt.Printf", ""},
		{"a call that takes no context", setup, "return ctx, nil", "component.report(cfg)\nreturn ctx, nil", ""},
		{"the dump on the suite's own context", setup, "dump(ctx, cfg)", "dump(ctx, cfg)\ndump(ctx, cfg)", ""},
		{"context.WithTimeout in place of within", setup, "within(ctx, suiteBounds.Install)", "context.WithTimeout(ctx, suiteBounds.Install)", "context.WithTimeout makes a context of install"},
		{"within with another field", setup, "suiteBounds.Install", "suiteBounds.Dump", "within makes a context of install"},
		{"within with a literal", setup, "suiteBounds.Install", "15*time.Minute", "within makes a context of install"},
		{"within with a bound of another value", setup, "suiteBounds.Install", "other.Install", "within makes a context of install"},
		{"within on a context of another origin", setup, "within(ctx", "within(context.Background()", "within makes a context of install"},
		{"a second context that within makes", setup, "for _, component", "again, stop := within(ctx, suiteBounds.Install)\nfor _, component", "within makes a context of install"},
		{"a context made from the bounded one", setup, "for _, component", "inner, stop := context.WithCancel(bounded)\nfor _, component", "context.WithCancel makes a context of install"},
		{"a context that within makes and nothing keeps", setup, "bounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n", "within(ctx, suiteBounds.Install)\n", "within makes a context of install"},
		{"a second wait on context.Background()", setup, "return ctx, nil", "if err := component.verify(context.Background(), cfg); err != nil {\nreturn ctx, err\n}\nreturn ctx, nil", "context.Background makes a context of install"},
		{"a context made once for each component, in a range", setup, "bounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\nfor _, component := range components {\n", "for _, component := range components {\nbounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n", "within makes a context of install"},
		{"a context made in a for", setup, "bounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n", "for i := 0; i < 3; i++ {\nbounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n}\n", "within makes a context of install"},
		{"a context made in a function literal", setup, "bounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n", "run := func() {\nbounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n}\n", "within makes a context of install"},
		{"a wait on the suite's own context", setup, "component.install(bounded, cfg)", "component.install(ctx, cfg)", "component.install is given the suite's own context"},
		{"the cause read from the suite's own context", setup, "context.Cause(bounded)", "context.Cause(ctx)", "context.Cause is given the suite's own context"},
		{"the bounded context replaced by the suite's own", setup, "for _, component", "bounded = ctx\nfor _, component", "bounded is assigned again"},
		{"no context made", setup, "bounded, cancel := within(ctx, suiteBounds.Install)\ndefer cancel()\n", "bounded := ctx\n", "install makes no context with within(ctx, suiteBounds.Install)"},
		{"no wait on the bounded context", setup, "component.install(bounded, cfg)", "component.install(cfg)", "no wait of install runs on the context that within(ctx, suiteBounds.Install) made"},
		{"a function that testenv.Setup is handed by another name", "other", "return ctx, nil", "return ctx, nil", "testenv.Setup is handed other, which the files declare no function of"},
		{"a function literal handed to testenv.Setup", "func() {}", "return ctx, nil", "return ctx, nil", "testenv.Setup is handed something but the name of a function"},
		{"nothing handed to testenv.Setup", "", "return ctx, nil", "return ctx, nil", "no file hands a function to testenv.Setup"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(theInstall, c.old) {
				t.Fatalf("the install has no %q to change", c.old)
			}
			body := strings.Replace(theInstall, c.old, c.new, 1)
			source := "package p\nfunc TestMain(m *testing.M) {\ntestenv.Setup(" + c.setup + ")\n}\nfunc install(ctx context.Context, cfg *envconf.Config) (context.Context, error) {\n" + body + "\n}\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheInstall(fset, []*ast.File{file}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestTheCheckOfRootContextsRefusesOneWhereverItIsMade(t *testing.T) {
	const root = " makes a context that no bound of the suite ends"
	cases := []struct {
		name, body, want string
	}{
		{"a context that within makes", "bounded, cancel := within(ctx, bounds.Apply)", ""},
		{"contexts that end by cancelling or carry a value", "a, cancel := context.WithCancel(ctx)\nb, cancel := context.WithCancelCause(ctx)\nc := context.WithValue(ctx, k, v)", ""},
		{"a context with a deadline", "bounded, cancel := context.WithTimeout(ctx, time.Minute)", ""},
		{"the cause of a context", "context.Cause(ctx)", ""},
		{"a root context of another package", "other.Background()", ""},
		{"context.Background", "available(context.Background(), cfg)", "context.Background" + root},
		{"context.TODO", "fresh := context.TODO()", "context.TODO" + root},
		{"context.WithoutCancel", "detached := context.WithoutCancel(ctx)", "context.WithoutCancel" + root},
		{"a root context in a function that a step calls", "func() {\nfresh := context.Background()\n}()", "context.Background" + root},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package p\nfunc helper(ctx context.Context) {\n"+c.body+"\n}\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithRootContexts(fset, []*ast.File{file}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestTheCheckOfRootContextsReadsEveryFileItIsGiven(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	for i, source := range []string{
		"package p\nfunc a(ctx context.Context) {\nwithin(ctx, bounds.Apply)\n}\n",
		"package p\nfunc b(ctx context.Context) {\ncontext.Background()\n}\n",
	} {
		file, err := parser.ParseFile(fset, fmt.Sprintf("%d.go", i), source, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	problems := problemsWithRootContexts(fset, files)
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "1.go:3:1: context.Background makes a context") {
		t.Fatalf("got %q", problems)
	}
}
