// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	namesItsCause   = []string{"apply", "applyTheCluster"}
	cutsShortInside = []string{"applyTheCluster"}
	derivesAContext = []string{"context.WithCancel", "context.WithCancelCause", "context.WithValue", "logr.NewContext"}
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
	return callee == "endAfter" || strings.HasPrefix(callee, "context.")
}

func repeats(n ast.Node) bool {
	switch n.(type) {
	case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
		return true
	}
	return false
}

var (
	callsThatMakeADeadline = []string{
		"context.WithTimeout", "context.WithTimeoutCause", "context.WithDeadline", "context.WithDeadlineCause",
		"wait.PollUntilContextTimeout", "wait.Poll", "wait.PollWithContext", "wait.PollImmediate", "wait.PollImmediateWithContext",
	}
	callsThatMakeARootContext = []string{"context.Background", "context.TODO", "context.WithoutCancel"}
)

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
				if callee != "endAfter" || !identNamed(argumentOf(n, 0), suite) || !isField || !isBoundsField(field, "bounds") || field.Sel.Name != "Install" || made == nil || bounded != "" || slices.ContainsFunc(stack, repeats) {
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
	scopeOf := func(stack []ast.Node, name string) ast.Node {
		for _, scope := range slices.Backward(stack) {
			if made[scope][name] {
				return scope
			}
		}
		return nil
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
			if !ok || !isIdent {
				break
			}
			switch callee := calleeOf(call); {
			case callee == "endAfter":
				bind(innermostFunction(stack), ident.Name)
			case slices.Contains(derivesAContext, callee):
				if parent, ok := argumentOf(call, 0).(*ast.Ident); ok {
					if scope := scopeOf(stack, parent.Name); scope != nil {
						bind(scope, ident.Name)
					}
				}
			}
		case *ast.CallExpr:
			callee := calleeOf(n)
			if callee == "cutShort" || callee == "endAfter" || strings.HasPrefix(callee, "context.") || slices.Contains(derivesAContext, callee) {
				break
			}
			var bounded string
			for _, arg := range n.Args {
				ident, ok := arg.(*ast.Ident)
				if ok && scopeOf(stack, ident.Name) != nil {
					bounded = ident.Name
					break
				}
			}
			switch {
			case bounded == "":
			case slices.Contains(namesItsCause, callee):
				if slices.Contains(cutsShortInside, callee) {
					checked++
				}
			default:
				checked++
				if !goesThroughCutShort(stack, bounded) {
					problems = append(problems, problemAt(fset, n, "the error of %s runs on %s, which endAfter ends, and does not go through cutShort(%s, err): the call is the init of an if, or is kept for the if that follows it, whose body calls cutShort(%s, err), or is an argument of cutShort(%s, …); a function that returns no error goes in namesItsCause; a helper that cuts short itself takes its context as its first parameter and goes in namesItsCause and cutsShortInside of endafter_test.go and, when it waits, in waitsOnTheCallersContext of scale_test.go", types.ExprString(n.Fun), bounded, bounded, bounded, bounded))
				}
			}
		}
		return true
	})
	return problems, checked
}

func problemsWithTheCutShortOfTheFiles(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	checked := make(map[string]int)
	wanted := []string{"suite_test.go", "steps_test.go", "join_test.go", "upgrade_test.go"}
	for _, file := range files {
		found, n := problemsWithTheCutShort(fset, file)
		problems = append(problems, found...)
		name := fset.Position(file.FileStart).Filename
		checked[name] = n
		if len(testsOf(file)) > 0 && !slices.Contains(wanted, name) {
			wanted = append(wanted, name)
		}
	}
	for _, name := range wanted {
		if checked[name] == 0 {
			problems = append(problems, fmt.Sprintf("%s holds no call on a context that endAfter made, and the install, the status patch before step 1, the move and the upgrade are such calls, and so is one in every file that declares a test", name))
		}
	}
	return problems
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
		switch container := stack[len(stack)-2].(type) {
		case *ast.IfStmt:
			return container.Init == parent && cutsShortAnErrorOf(container.Body, bounded, parent.Lhs)
		case *ast.BlockStmt:
			failure, ok := statementAfter(container, parent).(*ast.IfStmt)
			return ok && failure.Init == nil && cutsShortAnErrorOf(failure.Body, bounded, parent.Lhs)
		}
	}
	return false
}

func statementAfter(block *ast.BlockStmt, stmt ast.Stmt) ast.Stmt {
	at := slices.Index(block.List, stmt)
	if at+1 == len(block.List) {
		return nil
	}
	return block.List[at+1]
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
				if ident, ok := inner.(*ast.Ident); ok && slices.ContainsFunc(assigned, func(e ast.Expr) bool { return identNamed(e, ident.Name) }) {
					found = true
				}
			}
		}
	}
	return found
}

func problemsWithContextsOfTheirOwn(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	for _, file := range files {
		for n := range ast.Preorder(file) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				continue
			}
			switch callee := calleeOf(call); {
			case slices.Contains(callsThatMakeADeadline, callee):
				problems = append(problems, problemAt(fset, call, "%s ends by a deadline in a file of the suite, where every wait ends by cancelling a context that endAfter made, since a request under a deadline is refused by client-go's rate limiter with an error that is no context's", callee))
			case slices.Contains(callsThatMakeARootContext, callee):
				problems = append(problems, problemAt(fset, call, "%s makes a context that no bound of the suite ends, in a file where every context comes from the one that the step or the install is given", callee))
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

func TestTheInstallRunsItsWaitsOnAContextThatEndAfterEndsAtTheBoundOfTheInstall(t *testing.T) {
	fset, files := taggedFiles(t)
	for _, problem := range problemsWithTheInstall(fset, files) {
		t.Error(problem)
	}
}

func TestNoFileWithABuildTagMakesAContextOfItsOwn(t *testing.T) {
	fset, files := taggedFiles(t)
	for _, problem := range problemsWithContextsOfTheirOwn(fset, files) {
		t.Error(problem)
	}
}

func TestTheFailureOfACallOnAContextThatEndAfterMadeGoesThroughCutShort(t *testing.T) {
	fset, files := taggedFiles(t)
	for _, problem := range problemsWithTheCutShortOfTheFiles(fset, files) {
		t.Error(problem)
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
		{"a context that endAfter makes and nothing keeps", setup, "bounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n", "endAfter(ctx, bounds.Install)\n", "endAfter makes a context of install"},
		{"a second wait on context.Background()", setup, "return ctx, nil", "if err := component.verify(context.Background(), cfg); err != nil {\nreturn ctx, err\n}\nreturn ctx, nil", "context.Background makes a context of install"},
		{"a second wait on context.TODO() through a variable", setup, "return ctx, nil", "fresh := context.TODO()\nif err := component.verify(fresh, cfg); err != nil {\nreturn ctx, err\n}\nreturn ctx, nil", "context.TODO makes a context of install"},
		{"the only wait on context.Background()", setup, "component.install(bounded, cfg)", "component.install(context.Background(), cfg)", "context.Background makes a context of install"},
		{"endAfter with a bound of another value", setup, "bounds.Install", "other.Install", "endAfter makes a context of install"},
		{"a context made once for each component, in a range", setup, "bounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\nfor _, component := range components {\n", "for _, component := range components {\nbounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n", "endAfter makes a context of install"},
		{"a context made in a for", setup, "bounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n", "for i := 0; i < 3; i++ {\nbounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n}\n", "endAfter makes a context of install"},
		{"a context made in a function literal", setup, "bounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n", "run := func() {\nbounded, cancel := endAfter(ctx, bounds.Install)\ndefer cancel()\n}\n", "endAfter makes a context of install"},
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
		return "package p\nfunc TestX() {\nf := func(ctx context.Context, t *testing.T) {\n" + body + "\n}\n}\n"
	}
	inFunctionOf := func(name, parameters, body string) string {
		return "package p\nfunc " + name + "(" + parameters + ") error {\n" + body + "\n}\n"
	}
	inFunction := func(name, body string) string {
		return inFunctionOf(name, "ctx context.Context, cfg *envconf.Config", body)
	}
	inTwoSteps := func(first, second string) string {
		return "package p\nfunc TestX() {\nf := func(ctx context.Context, t *testing.T) {\n" + first + "\n}\ng := func(ctx context.Context, t *testing.T) {\n" + second + "\n}\n}\n"
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
		{"a call on a context derived by context.WithCancel through cutShort", inAStep(made + "inner, stop := context.WithCancel(bounded)\ndefer stop()\nif err := wait(inner); err != nil {\nt.Fatal(cutShort(inner, err))\n}"), ""},
		{"a call on a context derived by context.WithCancelCause through cutShort", inAStep(made + "inner, stop := context.WithCancelCause(bounded)\ndefer stop(nil)\nif err := wait(inner); err != nil {\nt.Fatal(cutShort(inner, err))\n}"), ""},
		{"a call on a context derived by context.WithValue through cutShort", inAStep(made + "inner := context.WithValue(bounded, key, value)\nif err := wait(inner); err != nil {\nt.Fatal(cutShort(inner, err))\n}"), ""},
		{"a call on a context derived by logr.NewContext through cutShort", inAStep(made + "inner := logr.NewContext(bounded, logger)\nif err := wait(inner); err != nil {\nt.Fatal(cutShort(inner, err))\n}"), ""},
		{"a call on a context derived from a derived one through cutShort", inAStep(made + "first := logr.NewContext(bounded, logger)\ninner, stop := context.WithCancel(first)\ndefer stop()\nif err := wait(inner); err != nil {\nt.Fatal(cutShort(inner, err))\n}"), ""},
		{"a call on a context derived from one that endAfter did not make", inAStep("inner := logr.NewContext(ctx, logger)\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), ""},
		{"a context that endAfter made in another step", inTwoSteps(made+"if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, err))\n}", "bounded, cancel := context.WithCancel(ctx)\nif err := wait(bounded); err != nil {\nt.Fatal(err)\n}"), ""},
		{"an error kept and cut short by the if that follows", inAStep(made + "err := wait(bounded)\nif err != nil {\nt.Fatal(cutShort(bounded, err))\n}"), ""},
		{"an error assigned to a variable and cut short by the if that follows", inAStep(made + "var err error\nerr = wait(bounded)\nif err != nil {\nt.Fatal(cutShort(bounded, fmt.Errorf(\"wait: %w\", err)))\n}"), ""},
		{"the cause of the bounded context in a log", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, err))\n}\nt.Logf(\"ended: %v\", context.Cause(bounded))"), ""},
		{"a call on a context derived by context.WithCancel as it is", inAStep(made + "inner, stop := context.WithCancel(bounded)\ndefer stop()\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on inner, which endAfter ends, and does not go through cutShort(inner, err)"},
		{"a call on a context derived by context.WithCancelCause as it is", inAStep(made + "inner, stop := context.WithCancelCause(bounded)\ndefer stop(nil)\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on inner"},
		{"a call on a context derived by context.WithValue as it is", inAStep(made + "inner := context.WithValue(bounded, key, value)\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on inner"},
		{"a call on a context derived by logr.NewContext as it is", inAStep(made + "inner := logr.NewContext(bounded, logger)\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on inner"},
		{"a call on a context derived from a derived one as it is", inAStep(made + "first := logr.NewContext(bounded, logger)\ninner, stop := context.WithCancel(first)\ndefer stop()\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on inner"},
		{"a call on a context derived in a closure of the step, as it is", inAStep(made + "var inner context.Context\nfunc() {\ninner = logr.NewContext(bounded, logger)\n}()\nif err := wait(inner); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on inner"},
		{"t.Fatal on the error as it is", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(err)\n}"), "the error of wait runs on bounded, which endAfter ends, and does not go through cutShort(bounded, err)"},
		{"t.Fatal on an error that wraps it", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(fmt.Errorf(\"wait: %w\", err))\n}"), "does not go through cutShort(bounded, err)"},
		{"t.FailNow after the error is logged", inAStep(made + "if err := wait(bounded); err != nil {\nt.Log(err)\nt.FailNow()\n}"), "does not go through cutShort(bounded, err)"},
		{"require on the error", inAStep(made + "if err := wait(bounded); err != nil {\nrequire.NoError(t, err)\n}"), "does not go through cutShort(bounded, err)"},
		{"require on an error kept for later", inAStep(made + "err := wait(bounded)\nrequire.NoError(t, err)"), "does not go through cutShort(bounded, err)"},
		{"an error kept and reported as it is by the if that follows", inAStep(made + "err := wait(bounded)\nif err != nil {\nt.Fatal(err)\n}"), "does not go through cutShort(bounded, err)"},
		{"an error kept and never tested", inAStep(made + "err := wait(bounded)"), "does not go through cutShort(bounded, err)"},
		{"an error kept and followed by an if that has an init of its own", inAStep(made + "err := wait(bounded)\nif err := other(); err != nil {\nt.Fatal(cutShort(bounded, err))\n}"), "does not go through cutShort(bounded, err)"},
		{"an error kept and followed by another statement", inAStep(made + "err := wait(bounded)\nt.Log(err)\nif err != nil {\nt.Fatal(cutShort(bounded, err))\n}"), "does not go through cutShort(bounded, err)"},
		{"cutShort on the step's own context", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(ctx, err))\n}"), "does not go through cutShort(bounded, err)"},
		{"cutShort on the step's own context that takes the call as it is", inAStep(made + "t.Fatal(cutShort(ctx, wait(bounded)))"), "does not go through cutShort(bounded, err)"},
		{"cutShort that does not take the error", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, errors.New(\"wait\")))\n}"), "does not go through cutShort(bounded, err)"},
		{"a call in a closure of the step", inAStep(made + "func() {\nif err := wait(bounded); err != nil {\nt.Fatal(err)\n}\n}()"), "does not go through cutShort(bounded, err)"},
		{"an error that the step returns as it is", inAStep(made + "return wait(bounded)"), "does not go through cutShort(bounded, err)"},
		{"a method of the receiver", inAStep(made + "if err := c.Move(bounded, options); err != nil {\nt.Fatal(err)\n}"), "the error of c.Move"},
		{"the bounded context as a later argument of a helper", inAStep(made + "aHelperThatNoFileDeclares(t, bounded, options)"), "a helper that cuts short itself takes its context as its first parameter and goes in namesItsCause and cutsShortInside of endafter_test.go and, when it waits, in waitsOnTheCallersContext of scale_test.go"},
		{"the patch through cutShort", inFunction("applyTheCluster", "return cutShort(ctx, "+cluster+")"), ""},
		{"an apply and then the patch through cutShort", inFunction("applyTheCluster", "if err := apply(ctx, cfg, manifest); err != nil {\nreturn err\n}\nreturn cutShort(ctx, "+cluster+")"), ""},
		{"a render that takes no context", inFunction("applyTheCluster", "manifest, err := render(file, name)\nreturn cutShort(ctx, "+cluster+")"), ""},
		{"the patch as it is", inFunction("applyTheCluster", "return "+cluster), "the error of k8s.Status().Patch runs on ctx, which endAfter ends, and does not go through cutShort(ctx, err)"},
		{"the patch in an if", inFunction("applyTheCluster", "if err := "+cluster+"; err != nil {\nreturn err\n}\nreturn nil"), "does not go through cutShort(ctx, err)"},
		{"another call on the context", inFunction("applyTheCluster", "if err := other(ctx); err != nil {\nreturn err\n}\nreturn cutShort(ctx, "+cluster+")"), "the error of other"},
		{"the patch through cutShort where the context is not the first parameter", inFunctionOf("applyTheCluster", "t *testing.T, ctx context.Context", "return cutShort(ctx, "+cluster+")"), ""},
		{"the patch as it is where the context is not the first parameter", inFunctionOf("applyTheCluster", "t *testing.T, ctx context.Context", "return "+cluster), "does not go through cutShort(ctx, err)"},
		{"the patch as it is where a cancel function comes first", inFunctionOf("applyTheCluster", "cancel context.CancelFunc, ctx context.Context", "return "+cluster), "does not go through cutShort(ctx, err)"},
		{"the patch as it is where a context of another package comes first", inFunctionOf("applyTheCluster", "request echo.Context, ctx context.Context", "return "+cluster), "does not go through cutShort(ctx, err)"},
		{"a function with no name for its context", "package p\nfunc applyTheCluster(context.Context, *envconf.Config) error {\nreturn nil\n}\n", ""},
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

func TestTheCheckOfCutShortCountsTheCallsItHolds(t *testing.T) {
	made := "bounded, cancel := endAfter(ctx, bounds.Apply)\ndefer cancel()\n"
	inAStep := func(body string) string {
		return "package p\nfunc TestX() {\nf := func(ctx context.Context, t *testing.T) {\n" + body + "\n}\n}\n"
	}
	cases := []struct {
		name, source string
		want         int
	}{
		{"a call that goes through cutShort", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, err))\n}"), 1},
		{"a call that does not", inAStep(made + "if err := wait(bounded); err != nil {\nt.Fatal(err)\n}"), 1},
		{"a function that cuts short itself, on the bounded context", inAStep(made + "if err := applyTheCluster(bounded, cfg, k8s, key); err != nil {\nt.Fatal(err)\n}"), 1},
		{"a function that cuts short itself, on a context that endAfter did not make", inAStep("if err := applyTheCluster(ctx, cfg, k8s, key); err != nil {\nt.Fatal(err)\n}"), 0},
		{"a function that names its cause itself", inAStep(made + "if err := apply(bounded, cfg, manifest); err != nil {\nt.Fatal(err)\n}"), 0},
		{"cutShort and endAfter", inAStep(made + "err = cutShort(bounded, err)"), 0},
		{"a context derived from the bounded one", inAStep(made + "inner, stop := context.WithCancel(bounded)\ndefer stop()\nfirst := logr.NewContext(inner, logger)\nt.Log(context.Cause(first))"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", c.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, got := problemsWithTheCutShort(fset, file); got != c.want {
				t.Fatalf("got %d calls, want %d", got, c.want)
			}
		})
	}
}

func TestTheCheckOfCutShortWantsACallOnAContextThatEndAfterMadeInEachFileItNames(t *testing.T) {
	const (
		held     = "package p\nfunc TestX() {\nf := func(ctx context.Context, t *testing.T) {\nbounded, cancel := endAfter(ctx, bounds.Apply)\nif err := wait(bounded); err != nil {\nt.Fatal(cutShort(bounded, err))\n}\n}\n}\n"
		none     = "package p\nfunc TestX() {}\n"
		helper   = "package p\nfunc helper() {}\n"
		mainOnly = "package p\nfunc TestMain(m *testing.M) {}\n"
	)
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a call in each", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": held}, nil},
		{"none in the install", map[string]string{"suite_test.go": none, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": held}, []string{"suite_test.go holds no call"}},
		{"none in the steps", map[string]string{"suite_test.go": held, "steps_test.go": none, "join_test.go": held, "upgrade_test.go": held}, []string{"steps_test.go holds no call"}},
		{"none in the join test", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": none, "upgrade_test.go": held}, []string{"join_test.go holds no call"}},
		{"none in the upgrade test", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": none}, []string{"upgrade_test.go holds no call"}},
		{"a file that is not read", map[string]string{"suite_test.go": held, "steps_test.go": held, "upgrade_test.go": held}, []string{"join_test.go holds no call"}},
		{"none in a file that declares no test and the check does not name", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": held, "install_test.go": helper}, nil},
		{"none in a file that declares TestMain alone and the check does not name", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": held, "other_test.go": mainOnly}, nil},
		{"a call in a file that declares a test and the check does not name", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": held, "minor_test.go": held}, nil},
		{"none in a file that declares a test and the check does not name", map[string]string{"suite_test.go": held, "steps_test.go": held, "join_test.go": held, "upgrade_test.go": held, "minor_test.go": none}, []string{"minor_test.go holds no call"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			var files []*ast.File
			for _, name := range slices.Sorted(maps.Keys(c.files)) {
				file, err := parser.ParseFile(fset, name, c.files[name], 0)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, file)
			}
			problems := problemsWithTheCutShortOfTheFiles(fset, files)
			if len(problems) != len(c.want) {
				t.Fatalf("got %q, want problems with %q", problems, c.want)
			}
			for i, want := range c.want {
				if !strings.Contains(problems[i], want) {
					t.Fatalf("want a problem with %q, got %q", want, problems[i])
				}
			}
		})
	}
}

func TestTheCheckOfContextsOfTheirOwnRefusesADeadlineAndARootContextWhereverTheyAreMade(t *testing.T) {
	const deadline = " ends by a deadline"
	const root = " makes a context that no bound of the suite ends"
	cases := []struct {
		name, body, want string
	}{
		{"a context that endAfter makes", "bounded, cancel := endAfter(ctx, bounds.Apply)", ""},
		{"contexts that end by cancelling or carry a value", "a, cancel := context.WithCancel(ctx)\nb, cancel := context.WithCancelCause(ctx)\nc := context.WithValue(ctx, k, v)", ""},
		{"a timeout that is not a context's", "helm.WithTimeout(\"5m\")", ""},
		{"a poll that ends with the context", "wait.PollUntilContextCancel(ctx, time.Second, true, f)\nwait.PollUntilWithContext(ctx, time.Second, f)\nwait.PollInfinite(time.Second, f)", ""},
		{"the test for an interrupted wait", "wait.Interrupted(err)", ""},
		{"a poll of another package", "other.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, f)\nother.Background()", ""},
		{"context.WithTimeout", "bounded, cancel := context.WithTimeout(ctx, time.Minute)", "context.WithTimeout" + deadline},
		{"context.WithDeadline", "bounded, cancel := context.WithDeadline(ctx, when)", "context.WithDeadline" + deadline},
		{"context.WithTimeoutCause", "bounded, cancel := context.WithTimeoutCause(ctx, time.Minute, err)", "context.WithTimeoutCause" + deadline},
		{"context.WithDeadlineCause", "bounded, cancel := context.WithDeadlineCause(ctx, when, err)", "context.WithDeadlineCause" + deadline},
		{"wait.PollUntilContextTimeout", "wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, f)", "wait.PollUntilContextTimeout" + deadline},
		{"wait.Poll", "wait.Poll(time.Second, time.Minute, f)", "wait.Poll" + deadline},
		{"wait.PollWithContext", "wait.PollWithContext(ctx, time.Second, time.Minute, f)", "wait.PollWithContext" + deadline},
		{"wait.PollImmediate", "wait.PollImmediate(time.Second, time.Minute, f)", "wait.PollImmediate" + deadline},
		{"wait.PollImmediateWithContext", "wait.PollImmediateWithContext(ctx, time.Second, time.Minute, f)", "wait.PollImmediateWithContext" + deadline},
		{"a deadline inside a function that a step calls", "func() {\nctx, cancel := context.WithTimeout(ctx, time.Minute)\n}()", "context.WithTimeout" + deadline},
		{"a poll with a timeout in a function that a step calls", "func() {\nwait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, f)\n}()", "wait.PollUntilContextTimeout" + deadline},
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
			problems := strings.Join(problemsWithContextsOfTheirOwn(fset, []*ast.File{file}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestTheCheckOfContextsOfTheirOwnReadsEveryFileItIsGiven(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	for i, source := range []string{
		"package p\nfunc a(ctx context.Context) {\nendAfter(ctx, bounds.Apply)\n}\n",
		"package p\nfunc b(ctx context.Context) {\nwait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, f)\ncontext.Background()\n}\n",
	} {
		file, err := parser.ParseFile(fset, fmt.Sprintf("%d.go", i), source, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	problems := problemsWithContextsOfTheirOwn(fset, files)
	if len(problems) != 2 || !strings.HasPrefix(problems[0], "1.go:3:1: wait.PollUntilContextTimeout ends by a deadline") || !strings.HasPrefix(problems[1], "1.go:4:1: context.Background makes a context") {
		t.Fatalf("got %q", problems)
	}
}
