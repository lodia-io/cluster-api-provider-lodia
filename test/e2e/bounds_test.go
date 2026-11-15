// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

type boundSet struct {
	Install, Dump                                                                                                time.Duration
	Apply, Initialized, Endpoint, Kindnet, Running, Ready, Konnectivity, Pod, Available, Move, Unpaused, Deleted time.Duration
	Deployed, ScaleUp, ScaleReady, ScaleAgents, ScaleDown, Rescheduled                                           time.Duration
	Owners, OwnersPaused, OwnersRestored, Finalizers, FinalizersPaused, FinalizersRestored, Settled, Stable      time.Duration
}

// bounds holds every wait of the suite. A step's own bound has to fail before
// go test gives up, since only the step's failure writes the artifacts.
var bounds = boundSet{
	Install:      15 * time.Minute,
	Dump:         2 * time.Minute,
	Apply:        time.Minute,
	Initialized:  4 * time.Minute,
	Endpoint:     30 * time.Second,
	Kindnet:      time.Minute,
	Running:      3 * time.Minute,
	Ready:        3 * time.Minute,
	Konnectivity: 2 * time.Minute,
	Pod:          2 * time.Minute,
	Available:    time.Minute,
	Move:         20 * time.Second,
	Unpaused:     30 * time.Second,
	Deployed:     time.Minute,
	ScaleUp:      3 * time.Minute,
	ScaleReady:   3 * time.Minute,
	ScaleAgents:  2 * time.Minute,
	ScaleDown:    3 * time.Minute,
	Rescheduled:  2 * time.Minute,
	Deleted:      3 * time.Minute,

	Owners:             30 * time.Second,
	OwnersPaused:       30 * time.Second,
	OwnersRestored:     2 * time.Minute,
	Finalizers:         30 * time.Second,
	FinalizersPaused:   30 * time.Second,
	FinalizersRestored: time.Minute,
	Settled:            2 * time.Minute,
	Stable:             2 * time.Minute,
}

type upgradeBoundSet struct {
	Apply, Initialized, Kindnet, Running, Ready, Available, Upgrade, Leader, Paused, Settled, Stable, Deleted time.Duration
}

var upgradeBounds = upgradeBoundSet{
	Apply:       time.Minute,
	Initialized: 4 * time.Minute,
	Kindnet:     time.Minute,
	Running:     3 * time.Minute,
	Ready:       3 * time.Minute,
	Available:   time.Minute,
	Upgrade:     4 * time.Minute,
	Leader:      2 * time.Minute,
	Paused:      30 * time.Second,
	Settled:     2 * time.Minute,
	Stable:      2 * time.Minute,
	Deleted:     3 * time.Minute,
}

const (
	// callsWithoutBound covers the calls between the waits, of which the first
	// to fail ends the test, and what a poll overshoots by.
	callsWithoutBound = 2 * time.Minute
	// outsideTheBinary is what the job spends beyond -timeout: its steps before
	// make e2e, the runner's disk among them, the build before the binary
	// starts, go test's minute of grace, and the removal of the containers and
	// the cluster.
	outsideTheBinary = 10 * time.Minute
)

const tenantRequestTimeout = 30 * time.Second

var dumpRequestTimeout = 10 * time.Second

// kube-apiserver ends a request that is not long-running at its own
// --request-timeout, 60 seconds by default, and ignores a longer timeout query.
const apiServerRequestTimeout = time.Minute

// The install's and the steps' client times out every request above the API
// server's own timeout, so that it never cuts a call the server would answer,
// a create that waits for webhooks among them.
var clientRequestTimeout = 75 * time.Second

// Controller-runtime's REST mapper discovers without the request's context,
// and three discovery requests of the client's timeout would not fit in
// callsWithoutBound.
var discoveryTimeout = 10 * time.Second

var tenantDumpBound = 30 * time.Second

var workerDumpBound = 30 * time.Second

var dockerCallBound = 30 * time.Second

var goTestTimeout = regexp.MustCompile(`-timeout (\d+)m\b`)

// A job of ci.yml runs one target of the Makefile, whose one go test line
// carries the build tags of the job's test and the -timeout the model fits
// under.
func targetTimeout(makefile []byte, target, tags string) (time.Duration, error) {
	rules := regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(target)+`:.*\n((?:\t.*(?:\n|$)|\n)*)`).FindAllSubmatch(makefile, -1)
	if rules == nil {
		return 0, fmt.Errorf("the Makefile has no target %s", target)
	}
	var lines [][]byte
	for _, rule := range rules {
		for line := range bytes.SplitSeq(rule[1], []byte("\n")) {
			if bytes.Contains(line, []byte("go test")) {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) != 1 {
		return 0, fmt.Errorf("the %s target has %d lines that run go test, want one", target, len(lines))
	}
	if !bytes.Contains(lines[0], []byte(" -tags "+tags+" ")) {
		return 0, fmt.Errorf("the %s target's go test does not run -tags %s: %s", target, tags, lines[0])
	}
	found := goTestTimeout.FindSubmatch(lines[0])
	if found == nil {
		return 0, fmt.Errorf("the %s target's go test has no -timeout in minutes: %s", target, lines[0])
	}
	n, err := strconv.Atoi(string(found[1]))
	return time.Duration(n) * time.Minute, err
}

func total(b any) time.Duration {
	var sum time.Duration
	for _, bound := range reflect.ValueOf(b).Fields() {
		sum += time.Duration(bound.Int())
	}
	return sum
}

func fits(bounded, timeout, limit time.Duration) error {
	if bounded+callsWithoutBound > timeout {
		return fmt.Errorf("the bounds add up to %s and the calls without one to %s, more than go test's -timeout %s: a late failure would hit the timeout, which writes no artifacts", bounded, callsWithoutBound, timeout)
	}
	if timeout+outsideTheBinary > limit {
		return fmt.Errorf("-timeout %s and the %s outside the test binary are more than the job's timeout-minutes %s", timeout, outsideTheBinary, limit)
	}
	return nil
}

// Nothing stops a discovery once the context has ended, and it asks /api,
// /apis and a group's version, each with the request timeout: a call can end
// three request timeouts after its bound. The first call of a client that a
// step opens between its waits asks its own request besides, a fourth.
func requestFits(whose string, requestTimeout time.Duration, requests int) error {
	if requestTimeout <= 0 || time.Duration(requests)*requestTimeout > callsWithoutBound {
		return fmt.Errorf("%s request timeout %s is not positive, or %d of them are more than the %s left for the calls without a bound: a call to a cluster that does not answer could overshoot its bound by that much", whose, requestTimeout, requests, callsWithoutBound)
	}
	return nil
}

// A call through the client discovers first, with up to three requests, and
// then takes its own request.
func clientFits(requestTimeout, discovery time.Duration) error {
	if requestTimeout <= apiServerRequestTimeout {
		return fmt.Errorf("the client's request timeout %s is not above the %s that kube-apiserver gives a request: it could cut a call that the server would answer", requestTimeout, apiServerRequestTimeout)
	}
	if err := requestFits("the steps' discovery", discovery, 3); err != nil {
		return err
	}
	if 3*discovery+requestTimeout > callsWithoutBound {
		return fmt.Errorf("a call through the client can take three discovery requests of %s and then its own of %s, more than the %s left for the calls without a bound", discovery, requestTimeout, callsWithoutBound)
	}
	return nil
}

// clusterctl's calls go through the suite's Proxy, with the client's two
// timeouts, and only their context ends what they ask. What they ask without one
// is a discovery: the version check of EnsureCustomResourceDefinitions, one
// request, and the first call of a client, up to three. The call after the
// version check runs in a retry that returns on an ended context before its
// first attempt, and a client asks nothing until its first call, so a call to a
// cluster that answers each of them just inside the timeout ends after its
// bound at most one client's three requests, never the version check besides;
// the model counts the four as margin. The requests with a context, which the
// client's timeout above the API server's keeps whole, end with it.
// ApplyUpgrade asks the same two without one, and its Proxy's ListResources
// discovers through the call's context, where clusterctl's own takes none.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/inventory.go#L146-L158
// https://github.com/kubernetes/apimachinery/blob/v0.37.2/pkg/util/wait/backoff.go#L494-L499
func clusterctlFits(discovery time.Duration) error {
	if discovery <= 0 || 4*discovery > callsWithoutBound {
		return fmt.Errorf("the discovery timeout %s of clusterctl's calls is not positive, or four of them are more than the %s left for the calls without a bound: a call of clusterctl to a cluster that answers each just in time could overshoot its bound by that much", discovery, callsWithoutBound)
	}
	return nil
}

// A step's context has no deadline, so docker ends every call at a bound of its
// own, and the first call to fail ends the test.
func dockerFits(bound time.Duration) error {
	if bound <= 0 || bound > callsWithoutBound {
		return fmt.Errorf("a docker call's bound %s is not positive, or is more than the %s left for the calls without a bound: a docker that hangs would hold the step past its own bound by that much", bound, callsWithoutBound)
	}
	return nil
}

// A pause that fails at its bound ends with one more call, the unpause, which
// has a bound of its own. The call that the bound cut short can run on by a
// discovery, and the unpause by another, which a context does not end.
func unpauseFits(bound, discovery time.Duration) error {
	if bound <= 0 || discovery <= 0 || bound+2*3*discovery > callsWithoutBound {
		return fmt.Errorf("the unpause's bound %s or the discovery timeout %s is not positive, or the unpause's bound and two discoveries of three requests, the one that its step's bound cuts short and its own, are more than the %s left for the calls without a bound: a cluster that stops answering would hold a pausing step past its bound by that much", bound, discovery, callsWithoutBound)
	}
	return nil
}

// The workers go first and a docker that hangs uses their whole bound: what
// remains of the dump has to hold the tenant's bound and as much again for the
// kind cluster's lists.
func tenantFits(requestTimeout, workerBound, dumpBound, dump time.Duration) error {
	if err := requestFits("the tenant's", requestTimeout, 4); err != nil {
		return err
	}
	if workerBound <= 0 || dumpBound <= 0 || workerBound+2*dumpBound >= dump {
		return fmt.Errorf("the workers' part of the dump %s or the tenant's %s is not positive, or the workers' and two of the tenant's are not less than the dump's %s: a docker that hangs would leave the kind cluster and the tenant too little time", workerBound, dumpBound, dump)
	}
	return nil
}

// A job's steps run its target alone, so that the -timeout and the
// timeout-minutes that the model is fitted to are the ones of the job's test.
func runsTarget(runs []string, target string) bool {
	return slices.ContainsFunc(runs, func(run string) bool { return strings.TrimSpace(run) == "make "+target })
}

type job struct {
	name, target, tags string
	bounded            time.Duration
}

var jobs = []job{
	{"e2e", "e2e", "e2e", total(bounds)},
	{"e2e-upgrade", "e2e-upgrade", "e2e,upgrade", bounds.Install + bounds.Dump + total(upgradeBounds)},
}

func TestTheBoundsFitUnderTheTimeouts(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var ci struct {
		Jobs map[string]struct {
			TimeoutMinutes int `json:"timeout-minutes"`
			Steps          []struct {
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		t.Run(j.name, func(t *testing.T) {
			timeout, err := targetTimeout(makefile, j.target, j.tags)
			if err != nil {
				t.Fatal(err)
			}
			limit := ci.Jobs[j.name]
			if limit.TimeoutMinutes == 0 {
				t.Fatalf("the %s job has no timeout-minutes", j.name)
			}
			var runs []string
			for _, step := range limit.Steps {
				runs = append(runs, step.Run)
			}
			if !runsTarget(runs, j.target) {
				t.Fatalf("the %s job does not run make %s: %q", j.name, j.target, runs)
			}
			if err := fits(j.bounded, timeout, time.Duration(limit.TimeoutMinutes)*time.Minute); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := tenantFits(tenantRequestTimeout, workerDumpBound, tenantDumpBound, bounds.Dump); err != nil {
		t.Fatal(err)
	}
	if err := requestFits("the dump's", dumpRequestTimeout, 3); err != nil {
		t.Fatal(err)
	}
	if err := clientFits(clientRequestTimeout, discoveryTimeout); err != nil {
		t.Fatal(err)
	}
	if err := clusterctlFits(discoveryTimeout); err != nil {
		t.Fatal(err)
	}
	if err := dockerFits(dockerCallBound); err != nil {
		t.Fatal(err)
	}
	if err := unpauseFits(unpauseBound, discoveryTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestTotalAddsEveryBound(t *testing.T) {
	for name, b := range map[string]any{"the join test's": &boundSet{}, "the upgrade test's": &upgradeBoundSet{}} {
		t.Run(name, func(t *testing.T) {
			var want time.Duration
			fields := reflect.ValueOf(b).Elem()
			for i := range fields.NumField() {
				d := time.Duration(1<<i) * time.Minute
				fields.Field(i).SetInt(int64(d))
				want += d
			}
			if got := total(reflect.ValueOf(b).Elem().Interface()); got != want {
				t.Fatalf("total is %s, the bounds add up to %s", got, want)
			}
		})
	}
}

func TestBoundsThatDoNotFitAreRefused(t *testing.T) {
	const minute = time.Minute
	cases := []struct {
		name                       string
		bounded, timeout, jobLimit time.Duration
		want                       string
	}{
		{"bounds of 49.5 minutes against 40", 49*minute + 30*time.Second, 40 * minute, 50 * minute, "more than go test's -timeout"},
		{"bounds that leave the calls no room", 39 * minute, 40 * minute, 50 * minute, "more than go test's -timeout"},
		{"bounds that leave the calls their room", 38 * minute, 40 * minute, 50 * minute, ""},
		{"a timeout the job cannot hold", 20 * minute, 45 * minute, 50 * minute, "more than the job's timeout-minutes"},
		{"a timeout one minute more than the job holds", 20 * minute, 41 * minute, 50 * minute, "more than the job's timeout-minutes"},
		{"a timeout the job holds", 20 * minute, 40 * minute, 50 * minute, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := fits(c.bounded, c.timeout, c.jobLimit)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestClientTimesThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name                      string
		requestTimeout, discovery time.Duration
		want                      string
	}{
		{"a request timeout of 75 seconds with a discovery of 10", 75 * second, 10 * second, ""},
		{"a request timeout one second above the API server's", 61 * second, 10 * second, ""},
		{"a request timeout equal to the API server's", 60 * second, 10 * second, "kube-apiserver"},
		{"a request timeout under the API server's", 30 * second, 10 * second, "kube-apiserver"},
		{"no request timeout", 0, 10 * second, "kube-apiserver"},
		{"a request timeout of 90 seconds with a discovery of 10", 90 * second, 10 * second, ""},
		{"a request timeout of 91 seconds with a discovery of 10", 91 * second, 10 * second, "then its own"},
		{"a request timeout of 75 seconds with a discovery of 15", 75 * second, 15 * second, ""},
		{"a request timeout of 75 seconds with a discovery of 16", 75 * second, 16 * second, "then its own"},
		{"a discovery of 41 seconds", 75 * second, 41 * second, "request timeout"},
		{"no discovery timeout", 75 * second, 0, "request timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := clientFits(c.requestTimeout, c.discovery)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestClusterctlTimesThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name      string
		discovery time.Duration
		want      string
	}{
		{"a discovery of 10 seconds", 10 * second, ""},
		{"a discovery of 30 seconds, four of which are the calls' whole room", 30 * second, ""},
		{"a discovery of 31 seconds", 31 * second, "four of them"},
		{"no discovery timeout", 0, "four of them"},
		{"a negative discovery timeout", -second, "four of them"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := clusterctlFits(c.discovery)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestDockerCallBoundsThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name  string
		bound time.Duration
		want  string
	}{
		{"a bound of 30 seconds", 30 * second, ""},
		{"a bound of the calls' whole room", callsWithoutBound, ""},
		{"a bound one second above the calls' room", callsWithoutBound + second, "docker call"},
		{"a bound of 5 minutes", 5 * time.Minute, "docker call"},
		{"no bound", 0, "docker call"},
		{"a negative bound", -second, "docker call"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := dockerFits(c.bound)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestUnpauseBoundsThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name             string
		bound, discovery time.Duration
		want             string
	}{
		{"a bound of 30 seconds with a discovery of 10", 30 * second, 10 * second, ""},
		{"a bound of 60 seconds with a discovery of 10, the calls' whole room", 60 * second, 10 * second, ""},
		{"a bound of 61 seconds with a discovery of 10", 61 * second, 10 * second, "the unpause's bound"},
		{"a bound of 30 seconds with a discovery of 15", 30 * second, 15 * second, ""},
		{"a bound of 30 seconds with a discovery of 16", 30 * second, 16 * second, "the unpause's bound"},
		{"no bound", 0, 10 * second, "the unpause's bound"},
		{"a negative bound", -second, 10 * second, "the unpause's bound"},
		{"no discovery timeout", 30 * second, 0, "the unpause's bound"},
		{"a negative discovery timeout", 30 * second, -second, "the unpause's bound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := unpauseFits(c.bound, c.discovery)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestTenantTimesThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name                                         string
		requestTimeout, workerBound, dumpBound, dump time.Duration
		want                                         string
	}{
		{"a request timeout of 30 seconds against 120, four of which fill it", 30 * second, 30 * second, 30 * second, 120 * second, ""},
		{"a request timeout of 31 seconds against 120", 31 * second, 30 * second, 30 * second, 120 * second, "request timeout"},
		{"no request timeout", 0, 30 * second, 30 * second, 120 * second, "request timeout"},
		{"a negative request timeout", -second, 30 * second, 30 * second, 120 * second, "request timeout"},
		{"a tenant dump of 44 seconds after workers of 30 against 120", 30 * second, 30 * second, 44 * second, 120 * second, ""},
		{"a tenant dump of 45 seconds after workers of 30 against 120", 30 * second, 30 * second, 45 * second, 120 * second, "part of the dump"},
		{"no tenant dump", 30 * second, 30 * second, 0, 120 * second, "part of the dump"},
		{"a negative tenant dump", 30 * second, 30 * second, -second, 120 * second, "part of the dump"},
		{"workers of 59 seconds before a tenant dump of 30 against 120", 30 * second, 59 * second, 30 * second, 120 * second, ""},
		{"workers of 60 seconds before a tenant dump of 30 against 120", 30 * second, 60 * second, 30 * second, 120 * second, "part of the dump"},
		{"no workers", 30 * second, 0, 30 * second, 120 * second, "part of the dump"},
		{"negative workers", 30 * second, -second, 30 * second, 120 * second, "part of the dump"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := tenantFits(c.requestTimeout, c.workerBound, c.dumpBound, c.dump)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestTheTimeoutIsTheOneOfTheTargetThatRunsTheTestsOfTheJob(t *testing.T) {
	const (
		suite   = "\tgo test -v -tags e2e -count=1 -timeout 64m ./test/e2e/...; status=$$?; \\\n"
		upgrade = "\tgo test -v -tags e2e,upgrade -count=1 -timeout 46m ./test/e2e/...; status=$$?; \\\n"
	)
	cases := []struct {
		name, makefile, target, tags string
		want                         time.Duration
		failure                      string
	}{
		{"the e2e target's alone", "e2e:\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"a target before it with a longer timeout", "test:\n\tgo test -timeout 99m ./...\ne2e:\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"a target before it with a shorter timeout", "test:\n\tgo test -timeout 5m ./...\ne2e:\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"a target after it with a timeout", "e2e:\n" + suite + "test:\n\tgo test -timeout 99m ./...\n", "e2e", "e2e", 64 * time.Minute, ""},
		{"the flags in another order", "e2e:\n\tgo test -timeout 30m -tags e2e ./test/e2e/...\n", "e2e", "e2e", 30 * time.Minute, ""},
		{"the last line without a newline", "e2e:\n\tgo test -tags e2e -timeout 30m ./test/e2e/...", "e2e", "e2e", 30 * time.Minute, ""},
		{"a variable of the target before its rule", "e2e-upgrade: E2E_CLUSTER = x\ne2e-upgrade: e2e-cluster\n" + upgrade, "e2e-upgrade", "e2e,upgrade", 46 * time.Minute, ""},
		{"a variable of the target after its rule", "e2e-upgrade: e2e-cluster\n" + upgrade + "e2e-upgrade: E2E_CLUSTER = x\n", "e2e-upgrade", "e2e,upgrade", 46 * time.Minute, ""},
		{"a comment that mentions the target", "# the e2e: target\ntest:\n\tgo test -timeout 5m -tags e2e ./...\ne2e:\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"a target with prerequisites and a comment", "e2e: e2e-cluster ## the suite\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"a blank line inside the recipe", "e2e:\n\techo a\n\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"the upgrade target after the e2e's", "e2e:\n" + suite + "e2e-upgrade:\n" + upgrade, "e2e-upgrade", "e2e,upgrade", 46 * time.Minute, ""},
		{"the e2e target before the upgrade's", "e2e:\n" + suite + "e2e-upgrade:\n" + upgrade, "e2e", "e2e", 64 * time.Minute, ""},
		{"the upgrade target before the e2e's", "e2e-upgrade:\n" + upgrade + "e2e:\n" + suite, "e2e", "e2e", 64 * time.Minute, ""},
		{"the upgrade's tags in the e2e target", "e2e:\n" + upgrade, "e2e", "e2e", 0, "does not run -tags e2e"},
		{"the e2e's tags in the upgrade target", "e2e-upgrade:\n" + suite, "e2e-upgrade", "e2e,upgrade", 0, "does not run -tags e2e,upgrade"},
		{"no timeout", "e2e:\n\tgo test -tags e2e ./test/e2e/...\n", "e2e", "e2e", 0, "no -timeout in minutes"},
		{"a timeout in seconds", "e2e:\n\tgo test -tags e2e -timeout 3600s ./test/e2e/...\n", "e2e", "e2e", 0, "no -timeout in minutes"},
		{"no target", "test:\n\tgo test ./...\n", "e2e", "e2e", 0, "no target e2e"},
		{"a target of a longer name only", "e2e-upgrade:\n" + upgrade, "e2e", "e2e", 0, "no target e2e"},
		{"no line for the suite", "e2e:\n\techo a\n", "e2e", "e2e", 0, "0 lines"},
		{"two lines for the suite", "e2e:\n" + suite + suite, "e2e", "e2e", 0, "2 lines"},
		{"a line of another target is not the target's", "e2e:\n\techo a\ntest:\n" + suite, "e2e", "e2e", 0, "0 lines"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := targetTimeout([]byte(c.makefile), c.target, c.tags)
			switch {
			case c.failure == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.failure != "" && (err == nil || !strings.Contains(err.Error(), c.failure)):
				t.Fatalf("want an error with %q, got %v", c.failure, err)
			case got != c.want:
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestAJobRunsItsTargetAlone(t *testing.T) {
	cases := []struct {
		name   string
		runs   []string
		target string
		want   bool
	}{
		{"the target", []string{"make e2e-lodia", "make e2e"}, "e2e", true},
		{"the target in a block", []string{"make e2e-lodia", "make e2e\n"}, "e2e", true},
		{"the target with another command", []string{"make e2e-lodia", "make e2e && true"}, "e2e", false},
		{"the target of a longer name only", []string{"make e2e-lodia", "make e2e-upgrade"}, "e2e", false},
		{"the upgrade's", []string{"make e2e-lodia", "make e2e-upgrade"}, "e2e-upgrade", true},
		{"no step that runs make", []string{"", "df -h /"}, "e2e", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runsTarget(c.runs, c.target); got != c.want {
				t.Errorf("got %t, want %t", got, c.want)
			}
		})
	}
}
