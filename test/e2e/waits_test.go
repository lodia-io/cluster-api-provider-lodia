// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	boundArgumentOf = map[string]int{
		"until":           2,
		"requireUnpaused": 3,
		"whilePaused":     3,
		"pauseCycle":      3,
		"settle":          3,
		"hold":            3,
		"within":          1,
	}
	contextThatEnds          = "within"
	waitsOnTheCallersContext = []string{
		"pollUntil", "apply", "applyTheCluster", "upgradeProvider", "installCertManager", "deploymentsAvailable",
		"clusterctlproxy.MoveUntilBlocked",
	}
	waitsAStepMayCall = []string{"docker", "clusterContainers", "kindnetOf", "deleteTheCluster", "requireGone"}
	waitsOfTheirOwn   = []string{
		"dump", "dumpBounded", "dumpTenant", "dumpWorkers", "dumpAll", "dumpCluster", "install", "installLodia", "installProviders", "ko", "toolOutput", "renderProvider", "lodiaProvider",
		"createWhenWebhooksAnswer", "gatedServer", "gatedTenant", "tenantThatNeverAnswers", "tenantThatNeverAnswersApis",
		"managementThatHolds", "requestsOfTheDump", "returnsWithin", "dockerCalls", "lodiaProviders",
	}
	waitsWithoutABound = []string{
		"time.Sleep", "time.After", "time.AfterFunc", "time.NewTimer", "time.NewTicker", "time.Tick",
		"context.WithTimeout", "context.WithDeadline", "context.WithTimeoutCause", "context.WithDeadlineCause",
	}
)

func calleeOf(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok {
			return pkg.Name + "." + fun.Sel.Name
		}
	}
	return ""
}

func isBoundsField(sel *ast.SelectorExpr, owner string) bool {
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == owner
}

func isAPoll(callee string) bool {
	return strings.HasPrefix(callee, "wait.") && callee != "wait.Interrupted"
}

func isAWait(callee string) bool {
	_, bounded := boundArgumentOf[callee]
	return bounded || slices.Contains(waitsOnTheCallersContext, callee) || slices.Contains(waitsAStepMayCall, callee) || slices.Contains(waitsOfTheirOwn, callee) || slices.Contains(waitsWithoutABound, callee) || isAPoll(callee)
}

// problemsWithTheWaits holds every wait of the Test functions of file to a
// field of the variable owner, of the set that fields lists, that no other wait
// takes, since total counts each field once, and every wait that takes its
// bound from the context it runs on to a context that a field of the owner ends
// in the same step. It refuses the waits that bound themselves but for the two
// that a step may call, whose bound the model holds per call (dockerFits), and
// the dump, which only a teardown may call, since the model counts
// suiteBounds.Dump once, and every field of suiteBounds, which bounds the
// install and the dump.
func problemsWithTheWaits(fset *token.FileSet, file *ast.File, owner string, fields []string) []string {
	var problems []string
	problem := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
	}
	var waits int
	used := make(map[string]int)
	taken := make(map[*ast.SelectorExpr]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		var path []ast.Node
		ended := make(map[ast.Node]map[string]bool)
		step := func() ast.Node {
			for _, n := range slices.Backward(path) {
				if _, ok := n.(*ast.FuncLit); ok {
					return n
				}
			}
			return nil
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				path = path[:len(path)-1]
				return true
			}
			path = append(path, n)
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						delete(ended[step()], ident.Name)
					}
				}
				if len(n.Rhs) != 1 {
					break
				}
				call, ok := n.Rhs[0].(*ast.CallExpr)
				if !ok || calleeOf(call) != contextThatEnds {
					break
				}
				ident, isIdent := n.Lhs[0].(*ast.Ident)
				if bound, isField := argumentOf(call, boundArgumentOf[calleeOf(call)]).(*ast.SelectorExpr); isIdent && isField && isBoundsField(bound, owner) {
					scope := step()
					if ended[scope] == nil {
						ended[scope] = make(map[string]bool)
					}
					ended[scope][ident.Name] = true
				}
			case *ast.CallExpr:
				callee := calleeOf(n)
				at, isWait := boundArgumentOf[callee]
				switch {
				case isWait:
					waits++
					if len(n.Args) <= at {
						problem(n, "%s takes a bound as its argument %d", callee, at+1)
						break
					}
					bound, ok := n.Args[at].(*ast.SelectorExpr)
					if !ok || !isBoundsField(bound, owner) {
						problem(n, "%s is bounded by something that is not a field of bounds", callee)
						break
					}
					taken[bound] = true
					used[bound.Sel.Name]++
				case slices.Contains(waitsOnTheCallersContext, callee):
					waits++
					if ctx, ok := argumentOf(n, 0).(*ast.Ident); !ok || !ended[step()][ctx.Name] {
						problem(n, "%s runs on a context that no field of bounds ends in this step", callee)
					}
				case callee == "dump" && inTheTeardown(path):
				case slices.Contains(waitsOfTheirOwn, callee):
					problem(n, "%s waits for a bound of its own, which the model of the bounds holds elsewhere and not for a step", callee)
				case slices.Contains(waitsWithoutABound, callee) || isAPoll(callee):
					problem(n, "%s waits without a field of bounds to bound it", callee)
				}
			case *ast.SelectorExpr:
				switch {
				case isBoundsField(n, "suiteBounds"):
					problem(n, "suiteBounds.%s bounds the install or the dump, which no step waits on", n.Sel.Name)
				case isBoundsField(n, owner) && !taken[n]:
					problem(n, "%s.%s is not the bound of a wait that the test knows", owner, n.Sel.Name)
				}
			}
			return true
		})
	}
	if waits == 0 {
		problems = append(problems, "the file holds no wait")
	}
	for _, field := range fields {
		if used[field] != 1 {
			problems = append(problems, fmt.Sprintf("%s.%s bounds %d waits of the file, want 1", owner, field, used[field]))
		}
	}
	return problems
}

func inTheTeardown(path []ast.Node) bool {
	for i, n := range path[:len(path)-1] {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Teardown" && slices.ContainsFunc(call.Args, func(arg ast.Expr) bool { return arg == path[i+1] }) {
			return true
		}
	}
	return false
}

func argumentOf(call *ast.CallExpr, at int) ast.Expr {
	if at >= len(call.Args) {
		return nil
	}
	return call.Args[at]
}

func knownWaits() []string {
	return slices.Concat(slices.Collect(maps.Keys(boundArgumentOf)), waitsOnTheCallersContext, waitsAStepMayCall, waitsOfTheirOwn)
}

func problemsWithTheFunctionsThatWait(fset *token.FileSet, files []*ast.File) []string {
	known := knownWaits()
	var problems []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || strings.HasPrefix(fn.Name.Name, "Test") || slices.Contains(known, fn.Name.Name) {
				continue
			}
			var reported bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && !reported && isAWait(calleeOf(call)) {
					reported = true
					problems = append(problems, fmt.Sprintf("%s: %s calls %s and waits, and the check of the waits does not know it: add it to boundArgumentOf, waitsOnTheCallersContext, waitsAStepMayCall or waitsOfTheirOwn", fset.Position(fn.Pos()), fn.Name.Name, calleeOf(call)))
				}
				return true
			})
		}
	}
	return problems
}

func fieldsOf(set any) []string {
	var names []string
	for field := range reflect.TypeOf(set).Fields() {
		names = append(names, field.Name)
	}
	return names
}

func requireEveryWaitBounded(t *testing.T, j job) {
	t.Helper()
	for field, bound := range reflect.ValueOf(j.set).Fields() {
		if bound.Int() <= 0 {
			t.Errorf("the bound %s is %s", field.Name, bound)
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, j.file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problemsWithTheWaits(fset, file, j.owner, fieldsOf(j.set)) {
		t.Error(problem)
	}
}

func TestEveryWaitOfEveryJobsFileIsBoundedByAFieldOfItsOwnBoundsThatNoOtherWaitTakes(t *testing.T) {
	for _, j := range jobs {
		t.Run(j.file, func(t *testing.T) {
			requireEveryWaitBounded(t, j)
		})
	}
}

func TestTheCheckOfTheWaitsRefusesWhatBreaksTheSumOfTheBounds(t *testing.T) {
	call := func(callee string, at int, bound string) string {
		args := make([]string, at+1)
		for i := range args {
			args[i] = "arg"
		}
		args[at] = bound
		return callee + "(" + strings.Join(args, ", ") + ")"
	}
	var all, twice []string
	field := make(map[string]string)
	for _, callee := range slices.Sorted(maps.Keys(boundArgumentOf)) {
		field[callee] = "bounds." + string(rune('A'+len(all)))
		all = append(all, call(callee, boundArgumentOf[callee], field[callee]))
		twice = append(twice, call(callee, boundArgumentOf[callee], "bounds.A"))
	}
	madeBy := func(maker string) string {
		made := slices.Clone(all)
		made[slices.Index(slices.Sorted(maps.Keys(boundArgumentOf)), maker)] = "bounded, cancel := " + call(maker, boundArgumentOf[maker], field[maker])
		return strings.Join(made, "\n")
	}
	var fields []string
	for i := range all {
		fields = append(fields, string(rune('A'+i)))
	}
	cases := []struct {
		name, body, want string
	}{
		{"every wait takes a field of its own", strings.Join(all, "\n"), ""},
		{"nothing waits", "", "holds no wait"},
		{"a field that bounds two waits", strings.Join(twice, "\n"), fmt.Sprintf("bounds.A bounds %d waits", len(twice))},
		{"a field that bounds no wait", strings.Join(all[1:], "\n"), "bounds.A bounds 0 waits"},
		{"the install's bound", strings.Join(all, "\n") + "\n" + call("until", 2, "suiteBounds.Install"), "suiteBounds.Install bounds the install or the dump"},
		{"the dump's bound, given to another call", strings.Join(all, "\n") + "\nother(ctx, suiteBounds.Dump)", "suiteBounds.Dump bounds the install or the dump"},
		{"a bound that is not a wait's", strings.Join(all, "\n") + "\nother(ctx, bounds.Z)", "bounds.Z is not the bound of a wait"},
		{"a field that bounds a wait, written as a sum", strings.Join(all[1:], "\n") + "\nuntil(ctx, t, bounds.A+time.Second)", "is bounded by something that is not a field of bounds"},
		{"a wait without a bound", strings.Join(all, "\n") + "\nuntil(ctx, t)", "until takes a bound as its argument 3"},
		{"a wait with a field of another value", strings.Join(all[1:], "\n") + "\nuntil(ctx, t, other.A)", "is bounded by something that is not a field of bounds"},
		{"a poll of apimachinery's wait package", strings.Join(all, "\n") + "\nwait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, f)", "wait.PollUntilContextTimeout waits without a field"},
		{"apimachinery's test for an interrupted wait", strings.Join(all, "\n") + "\nwait.Interrupted(err)", ""},
	}
	for _, callee := range waitsOnTheCallersContext {
		cases = append(cases,
			struct{ name, body, want string }{callee + " on the step's own context", strings.Join(all, "\n") + "\n" + callee + "(ctx)", callee + " runs on a context that no field of bounds ends in this step"},
			struct{ name, body, want string }{callee + " on a context made in another step", "func() {\n" + madeBy("within") + "\n}()\nfunc() {\n" + callee + "(bounded)\n}()", callee + " runs on a context that no field of bounds ends in this step"},
			struct{ name, body, want string }{callee + " on a context that context.WithTimeout made from a field", strings.Join(all, "\n") + "\nbounded, cancel := context.WithTimeout(ctx, bounds.A)\n" + callee + "(bounded)", callee + " runs on a context that no field of bounds ends in this step"},
			struct{ name, body, want string }{callee + " on a context made from a literal", strings.Join(all[1:], "\n") + "\nbounded, cancel := within(ctx, 5*time.Minute)\n" + callee + "(bounded)", callee + " runs on a context that no field of bounds ends in this step"},
			struct{ name, body, want string }{callee + " on a context made by " + contextThatEnds, madeBy(contextThatEnds) + "\n" + callee + "(bounded)", ""},
			struct{ name, body, want string }{callee + " on a context made by " + contextThatEnds + " and then replaced by the step's own", madeBy(contextThatEnds) + "\nbounded = ctx\n" + callee + "(bounded)", callee + " runs on a context that no field of bounds ends in this step"},
		)
	}
	for _, callee := range waitsOfTheirOwn {
		cases = append(cases, struct{ name, body, want string }{callee + " in a step", strings.Join(all, "\n") + "\n" + callee + "()", callee + " waits for a bound of its own"})
	}
	for _, callee := range waitsAStepMayCall {
		cases = append(cases, struct{ name, body, want string }{callee + " in a step", strings.Join(all, "\n") + "\n" + callee + "()", ""})
	}
	inAFeature := func(steps, teardown string) string {
		return strings.Join(all, "\n") + "\nfeatures.New(\"a\").Assess(\"b\", func() {\n" + steps + "\n}).Teardown(func() {\n" + teardown + "\n}).Feature()"
	}
	cases = append(cases,
		struct{ name, body, want string }{"dump in the teardown", inAFeature("", "dump()"), ""},
		struct{ name, body, want string }{"dump in a closure of the teardown", inAFeature("", "if t.Failed() {\nfunc() { dump() }()\n}"), ""},
		struct{ name, body, want string }{"dump in a step of a feature that has a teardown", inAFeature("dump()", ""), "dump waits for a bound of its own"},
		struct{ name, body, want string }{"dump in a step and in the teardown", inAFeature("dump()", "dump()"), "dump waits for a bound of its own"},
		struct{ name, body, want string }{"a function of its own in the teardown", inAFeature("", "dumpAll()"), "dumpAll waits for a bound of its own"},
	)
	for _, callee := range slices.Sorted(maps.Keys(boundArgumentOf)) {
		at := boundArgumentOf[callee]
		var others []string
		for other, otherAt := range boundArgumentOf {
			if other != callee {
				others = append(others, call(other, otherAt, "bounds."+string(rune('B'+len(others)))))
			}
		}
		cases = append(cases, struct{ name, body, want string }{
			callee + " bounded by a literal", call(callee, at, "5*time.Minute") + "\n" + strings.Join(others, "\n"), callee + " is bounded by something that is not a field of bounds",
		})
	}
	for _, callee := range []string{
		"time.Sleep", "time.After", "time.AfterFunc", "time.NewTimer", "time.NewTicker", "time.Tick",
		"context.WithTimeout", "context.WithDeadline", "context.WithTimeoutCause", "context.WithDeadlineCause",
	} {
		cases = append(cases, struct{ name, body, want string }{callee, strings.Join(all, "\n") + "\n" + callee + "()", callee + " waits without a field"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := "package p\nfunc TestX() {\n" + c.body + "\n}\nfunc until() { wait.PollUntilContextTimeout(); time.Sleep() }\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheWaits(fset, file, "bounds", fields), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestTheCheckOfTheWaitsHoldsAFileToTheFieldsOfItsOwnBounds(t *testing.T) {
	onlyA, aAndB := []string{"A"}, []string{"A", "B"}
	cases := []struct {
		name, owner, body string
		fields            []string
		want              string
	}{
		{"a wait bounded by a field of the file's own set", "upgradeBounds", "until(ctx, t, upgradeBounds.A, w, f)", onlyA, ""},
		{"a wait bounded by a field of another set", "upgradeBounds", "until(ctx, t, bounds.A, w, f)", onlyA, "is bounded by something that is not a field of bounds"},
		{"a wait bounded by a field of the join test's set in the join test", "bounds", "until(ctx, t, bounds.A, w, f)", onlyA, ""},
		{"a wait bounded by a field of the upgrade's set in the join test", "bounds", "until(ctx, t, upgradeBounds.A, w, f)", onlyA, "is bounded by something that is not a field of bounds"},
		{"a field of the file's own set that bounds nothing", "upgradeBounds", "until(ctx, t, upgradeBounds.A, w, f)\nother(ctx, upgradeBounds.A)", onlyA, "upgradeBounds.A is not the bound of a wait"},
		{"a field of another set that bounds nothing", "upgradeBounds", "until(ctx, t, upgradeBounds.A, w, f)\nother(ctx, bounds.A)", onlyA, ""},
		{"a wait that takes the suite's bound of the install", "upgradeBounds", "until(ctx, t, upgradeBounds.A, w, f)\nuntil(ctx, t, suiteBounds.Install, w, f)", onlyA, "suiteBounds.Install bounds the install or the dump"},
		{"a field that bounds no wait of the file", "upgradeBounds", "until(ctx, t, upgradeBounds.A, w, f)", aAndB, "upgradeBounds.B bounds 0 waits of the file, want 1"},
		{"a wait on a context that a field of the file's own set ends", "upgradeBounds", "bounded, cancel := within(ctx, upgradeBounds.A)\npollUntil(bounded)", onlyA, ""},
		{"a wait on a context that a field of another set ends", "upgradeBounds", "bounded, cancel := within(ctx, bounds.A)\npollUntil(bounded)", onlyA, "pollUntil runs on a context that no field of bounds ends in this step"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := "package p\nfunc TestX() {\n" + c.body + "\n}\nfunc until() { wait.PollUntilContextTimeout(); time.Sleep() }\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheWaits(fset, file, c.owner, c.fields), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestTheFieldsOfASetAreItsOwn(t *testing.T) {
	if got := fieldsOf(struct{ A, B, C time.Duration }{}); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Errorf("got %v", got)
	}
	if got := fieldsOf(boundSet{}); slices.Contains(got, "Install") || slices.Contains(got, "Dump") || !slices.Contains(got, "Stable") {
		t.Errorf("got %v", got)
	}
	if got := fieldsOf(suiteBoundSet{}); !slices.Equal(got, []string{"Install", "Dump"}) {
		t.Errorf("got %v", got)
	}
}

func TestTheCheckOfTheFunctionsThatWaitRefusesWhatItDoesNotKnow(t *testing.T) {
	cases := []struct {
		name, source, want string
	}{
		{"a helper that polls", "func helper() { wait.PollUntilContextCancel() }", "helper calls wait.PollUntilContextCancel"},
		{"a helper that sleeps", "func helper() { time.Sleep() }", "helper calls time.Sleep"},
		{"a helper that makes a ticker in a loop", "func helper() { for { time.NewTicker() } }", "helper calls time.NewTicker"},
		{"a helper that makes a context with a timeout", "func helper() { context.WithTimeout() }", "helper calls context.WithTimeout"},
		{"a helper that ends a context", "func helper() { within() }", "helper calls within"},
		{"a helper that calls a wait that takes its bound", "func helper() { settle() }", "helper calls settle"},
		{"a helper that calls a wait on its caller's context", "func helper() { pollUntil() }", "helper calls pollUntil"},
		{"a helper that calls a wait of clusterctlproxy", "func helper() { clusterctlproxy.MoveUntilBlocked() }", "helper calls clusterctlproxy.MoveUntilBlocked"},
		{"a method that waits", "func (r *r) helper() { time.After() }", "helper calls time.After"},
		{"a helper that calls docker", "func helper() { docker() }", "helper calls docker"},
		{"a helper that calls the dump", "func helper() { dump() }", "helper calls dump"},
		{"a helper that calls a wait of its own", "func helper() { install() }", "helper calls install"},
		{"a helper that runs a tool", "func helper() { toolOutput() }", "helper calls toolOutput"},
		{"a helper that waits in a closure", "func helper() { f := func() { time.After() }; f() }", "helper calls time.After"},
		{"a wait that takes its bound", "func until() { wait.PollUntilContextCancel() }", ""},
		{"a wait on its caller's context", "func pollUntil() { wait.PollUntilContextCancel() }", ""},
		{"a wait of its own", "func docker() { context.WithTimeout() }", ""},
		{"a Test function", "func TestX() { time.Sleep() }", ""},
		{"a helper that does not wait", "func helper() { time.Now(); time.Since(); time.Unix(); wait.Interrupted() }", ""},
		{"a declaration without a body", "func helper()", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package p\n"+c.source+"\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheFunctionsThatWait(fset, []*ast.File{file}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestEveryFunctionOfThePackageThatWaitsIsOneThatTheCheckOfTheWaitsKnows(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	declared := make(map[string]bool)
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				declared[fn.Name.Name] = true
			}
		}
	}
	for _, problem := range problemsWithTheFunctionsThatWait(fset, files) {
		t.Error(problem)
	}
	for _, name := range knownWaits() {
		if !strings.Contains(name, ".") && !declared[name] {
			t.Errorf("the check of the waits knows %s, which the package does not declare", name)
		}
	}
}

func TestTheFunctionsThatWaitThroughHelmAndClusterctlAreOnesNoStepMayCall(t *testing.T) {
	for _, name := range []string{"installLodia", "installProviders", "ko", "toolOutput"} {
		if !slices.Contains(waitsOfTheirOwn, name) {
			t.Errorf("%s waits through calls that the check of the waits does not know by name, so it has to be in waitsOfTheirOwn", name)
		}
	}
}
