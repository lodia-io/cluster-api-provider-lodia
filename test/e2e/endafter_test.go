// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	namesItsCause   = []string{"apply", "applyTheCluster"}
	cutsShortInside = []string{"applyTheCluster"}
)

func identNamed(e ast.Expr, name string) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == name
}

func contextParameterOf(fn *ast.FuncDecl) string {
	if len(fn.Type.Params.List) == 0 || len(fn.Type.Params.List[0].Names) == 0 {
		return ""
	}
	return fn.Type.Params.List[0].Names[0].Name
}

func isAContextMaker(callee string) bool {
	return callee == "endAfter" || strings.HasPrefix(callee, "context.With")
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
				if callee != "endAfter" || !identNamed(argumentOf(n, 0), suite) || !isField || !isBoundsField(field, "bounds") || field.Sel.Name != "Install" || made == nil || bounded != "" {
					problems = append(problems, problemAt(fset, n, "%s makes a context of %s, whose one context has to be made by endAfter(%s, bounds.Install)", callee, fn.Name.Name, suite))
					break
				}
				bounded = made.Name
			case identNamed(argumentOf(n, 0), suite) && callee != "dump":
				problems = append(problems, problemAt(fset, n, "%s is given the suite's own context, which no bound ends, in %s", callee, fn.Name.Name))
			case bounded != "" && identNamed(argumentOf(n, 0), bounded) && callee != "cutShort":
				waits++
			}
		}
		return true
	})
	switch {
	case bounded == "":
		problems = append(problems, problemAt(fset, fn, "%s makes no context with endAfter(%s, bounds.Install) for its waits to run on", fn.Name.Name, suite))
	case waits == 0:
		problems = append(problems, problemAt(fset, fn, "no wait of %s runs on the context that endAfter(%s, bounds.Install) made", fn.Name.Name, suite))
	}
	return problems
}

func problemsWithTheCutShort(fset *token.FileSet, file *ast.File) ([]string, int) {
	var problems []string
	var checked int
	made := make(map[ast.Node]map[string]bool)
	bind := func(scope ast.Node, name string) {
		if made[scope] == nil {
			made[scope] = make(map[string]bool)
		}
		made[scope][name] = true
	}
	ast.PreorderStack(file, nil, func(n ast.Node, stack []ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if slices.Contains(cutsShortInside, n.Name.Name) && n.Body != nil {
				bind(n, contextParameterOf(n))
			}
		case *ast.AssignStmt:
			call, ok := singleCall(n)
			ident, isIdent := n.Lhs[0].(*ast.Ident)
			if ok && isIdent && calleeOf(call) == "endAfter" {
				bind(innermostFunction(stack), ident.Name)
			}
		case *ast.CallExpr:
			callee := calleeOf(n)
			if callee == "cutShort" || callee == "endAfter" || slices.Contains(namesItsCause, callee) {
				break
			}
			var bounded string
			for _, arg := range n.Args {
				ident, ok := arg.(*ast.Ident)
				if ok && slices.ContainsFunc(stack, func(scope ast.Node) bool { return made[scope][ident.Name] }) {
					bounded = ident.Name
					break
				}
			}
			if bounded == "" {
				break
			}
			checked++
			if !goesThroughCutShort(stack, bounded) {
				problems = append(problems, problemAt(fset, n, "the error of %s runs on %s, which endAfter ends, and does not go through cutShort(%s, err)", types.ExprString(n.Fun), bounded, bounded))
			}
		}
		return true
	})
	return problems, checked
}

func innermostFunction(stack []ast.Node) ast.Node {
	for _, n := range slices.Backward(stack) {
		switch n.(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return n
		}
	}
	return nil
}

func singleCall(assign *ast.AssignStmt) (*ast.CallExpr, bool) {
	if len(assign.Rhs) != 1 {
		return nil, false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	return call, ok
}

func goesThroughCutShort(stack []ast.Node, bounded string) bool {
	switch parent := stack[len(stack)-1].(type) {
	case *ast.CallExpr:
		return calleeOf(parent) == "cutShort" && identNamed(argumentOf(parent, 0), bounded)
	case *ast.AssignStmt:
		if len(stack) < 2 {
			return false
		}
		failure, ok := stack[len(stack)-2].(*ast.IfStmt)
		return ok && failure.Init == parent && cutsShortAnErrorOf(failure.Body, bounded, parent.Lhs)
	}
	return false
}

func cutsShortAnErrorOf(body *ast.BlockStmt, bounded string, assigned []ast.Expr) bool {
	var found bool
	for n := range ast.Preorder(body) {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeOf(call) != "cutShort" || !identNamed(argumentOf(call, 0), bounded) || len(call.Args) < 2 {
			continue
		}
		for _, arg := range call.Args[1:] {
			for inner := range ast.Preorder(arg) {
				if ident, ok := inner.(*ast.Ident); ok && ident.Name != "_" && slices.ContainsFunc(assigned, func(e ast.Expr) bool { return identNamed(e, ident.Name) }) {
					found = true
				}
			}
		}
	}
	return found
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

func TestTheInstallRunsItsWaitsOnAContextThatEndAfterEndsAtTheBoundOfTheInstall(t *testing.T) {
	fset, files := taggedFiles(t)
	for _, problem := range problemsWithTheInstall(fset, files) {
		t.Error(problem)
	}
}

func TestTheFailureOfACallOnAContextThatEndAfterMadeGoesThroughCutShort(t *testing.T) {
	fset, files := taggedFiles(t)
	checked := make(map[string]int)
	for _, file := range files {
		problems, n := problemsWithTheCutShort(fset, file)
		checked[fset.Position(file.FileStart).Filename] = n
		for _, problem := range problems {
			t.Error(problem)
		}
	}
	for _, name := range []string{"suite_test.go", "steps_test.go", "join_test.go", "upgrade_test.go"} {
		if checked[name] == 0 {
			t.Errorf("%s holds no call on a context that endAfter made, and the install, the status patch before step 1, the move and the upgrade are such calls", name)
		}
	}
}

const theInstall = `bounded, cancel := endAfter(ctx, bounds.Install)
defer cancel()
for _, component := range components {
	start := time.Now()
	if err := component.install(bounded, cfg); err != nil {
		dump(ctx, cfg)
		return ctx, fmt.Errorf("install %s: %w", component.name, cutShort(bounded, err))
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
		{"context.WithTimeout in place of endAfter", setup, "endAfter(ctx, bounds.Install)", "context.WithTimeout(ctx, bounds.Install)", "context.WithTimeout makes a context of install"},
		{"endAfter with another field", setup, "bounds.Install", "bounds.Dump", "endAfter makes a context of install"},
		{"endAfter with a literal", setup, "bounds.Install", "15*time.Minute", "endAfter makes a context of install"},
		{"endAfter on a context of another origin", setup, "endAfter(ctx", "endAfter(context.Background()", "endAfter makes a context of install"},
		{"a second context that endAfter makes", setup, "for _, component", "again, stop := endAfter(ctx, bounds.Install)\nfor _, component", "endAfter makes a context of install"},
		{"a context made from the bounded one", setup, "for _, component", "inner, stop := context.WithCancel(bounded)\nfor _, component", "context.WithCancel makes a context of install"},
		{"a wait on the suite's own context", setup, "component.install(bounded, cfg)", "component.install(ctx, cfg)", "component.install is given the suite's own context"},
		{"cutShort on the suite's own context", setup, "cutShort(bounded, err)", "cutShort(ctx, err)", "cutShort is given the suite's own context"},
		{"the bounded context replaced by the suite's own", setup, "for _, component", "bounded = ctx\nfor _, component", "bounded is assigned again"},
		{"no context made", setup, "bounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n", "bounded := ctx\n", "install makes no context with endAfter(ctx, bounds.Install)"},
		{"no wait on the bounded context", setup, "component.install(bounded, cfg)", "component.install(cfg)", "no wait of install runs on the context that endAfter(ctx, bounds.Install) made"},
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

func TestTheCheckOfCutShortRefusesAFailureThatReachesItsReportAsItIs(t *testing.T) {
	made := "bounded, cancel := endAfter(ctx, bounds.Apply)\ndefer cancel()\n"
	inAStep := func(body string) string {
		return "package p\nfunc TestX() {\nf := func(ctx context.Context, t *testing.T) context.Context {\n" + body + "\nreturn ctx\n}\n}\n"
	}
	inFunction := func(name, body string) string {
		return "package p\nfunc " + name + "(ctx context.Context, cfg *envconf.Config) error {\n" + body + "\n}\n"
	}
	const cluster = "k8s.Status().Patch(ctx, provisioned, patch)"
	cases := []struct {
		name, source, want string
	}{
		{"a failure that goes through cutShort", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, err))\n}"), ""},
		{"an error that cutShort wraps", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, fmt.Errorf(\"wait: %w\", err)))\n}"), ""},
		{"a call that cutShort takes as it is", inAStep(made + "t.Fatal(cutShort(bounded, wait(bounded)))"), ""},
		{"a failure reported by a return through fmt.Errorf", inAStep(made + "if err := wait(bounded); err != nil {\nreturn ctx, fmt.Errorf(\"wait: %w\", cutShort(bounded, err))\n}"), ""},
		{"a failure on the step's own context reported as it is", inAStep(made + "if err := wait(ctx); err != nil {\nt.Fatal(err)\n}"), ""},
		{"an apply on the bounded context, which names its cause itself", inAStep(made + "if err := apply(bounded, cfg, manifest); err != nil {\nt.Fatal(err)\n}"), ""},
		{"applyTheCluster on the bounded context", inAStep(made + "if err := applyTheCluster(bounded, cfg, k8s, key); err != nil {\nt.Fatal(err)\n}"), ""},
		{"a context that endAfter did not make", inAStep("bounded, cancel := context.WithCancel(ctx)\nif err := wait(bounded); err != nil {\nt.Fatal(err)\n}"), ""},
		{"t.Fatal on the error as it is", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on bounded, which endAfter ends, and does not go through cutShort(bounded, err)"},
		{"t.Fatal on an error that wraps it", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(fmt.Errorf(\"wait: %w\", err))\n}"), "does not go through cutShort(bounded, err)"},
		{"t.FailNow after the error is logged", inAStep(made + "if err := wait(bounded); err != nil {\nt.Log(err)\nt.FailNow()\n}"), "does not go through cutShort(bounded, err)"},
		{"require on the error", inAStep(made + "if err := wait(bounded); err != nil {\nrequire.NoError(t, err)\n}"), "does not go through cutShort(bounded, err)"},
		{"require on an error kept for later", inAStep(made + "err := wait(bounded)\nrequire.NoError(t, err)"), "does not go through cutShort(bounded, err)"},
		{"cutShort on the step's own context", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(ctx, err))\n}"), "does not go through cutShort(bounded, err)"},
		{"cutShort that does not take the error", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, errors.New(\"wait\")))\n}"), "does not go through cutShort(bounded, err)"},
		{"a call in a closure of the step", inAStep(made + "func() {\nif err := wait(bounded); err != nil {\nt.Fatal(err)\n}\n}()"), "does not go through cutShort(bounded, err)"},
		{"an error that the step returns as it is", inAStep(made + "return wait(bounded)"), "does not go through cutShort(bounded, err)"},
		{"a method of the receiver", inAStep(made + "if err := c.Move(bounded, options); err != nil {\nt.Fatal(err)\n}"), "the error of c.Move"},
		{"the patch through cutShort", inFunction("applyTheCluster", "return cutShort(ctx, "+cluster+")"), ""},
		{"an apply and then the patch through cutShort", inFunction("applyTheCluster", "if err := apply(ctx, cfg, manifest); err != nil {\nreturn err\n}\nreturn cutShort(ctx, "+cluster+")"), ""},
		{"a render that takes no context", inFunction("applyTheCluster", "manifest, err := render(file, name)\nreturn cutShort(ctx, "+cluster+")"), ""},
		{"the patch as it is", inFunction("applyTheCluster", "return "+cluster), "the error of k8s.Status().Patch runs on ctx, which endAfter ends, and does not go through cutShort(ctx, err)"},
		{"the patch in an if", inFunction("applyTheCluster", "if err := "+cluster+"; err != nil {\nreturn err\n}\nreturn nil"), "does not go through cutShort(ctx, err)"},
		{"another call on the context", inFunction("applyTheCluster", "if err := other(ctx); err != nil {\nreturn err\n}\nreturn cutShort(ctx, "+cluster+")"), "the error of other"},
		{"the patch as it is in a function that cuts nothing short", inFunction("helper", "return "+cluster), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", c.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			found, _ := problemsWithTheCutShort(fset, file)
			problems := strings.Join(found, "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}
