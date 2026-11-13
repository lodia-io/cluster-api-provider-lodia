// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
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

const (
	// callsWithoutBound covers the calls between the waits, of which the first
	// to fail ends the test, and what a poll overshoots by.
	callsWithoutBound = 2 * time.Minute
	// outsideTheBinary is what the job spends beyond -timeout: the build before
	// the binary starts, go test's minute of grace, and the removal of the
	// containers and the cluster.
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

func total(b boundSet) time.Duration {
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
// three request timeouts after its bound.
func requestFits(whose string, requestTimeout time.Duration) error {
	if requestTimeout <= 0 || 3*requestTimeout > callsWithoutBound {
		return fmt.Errorf("%s request timeout %s is not positive, or three of them are more than the %s left for the calls without a bound: a call to a cluster that does not answer could overshoot its bound by that much", whose, requestTimeout, callsWithoutBound)
	}
	return nil
}

// A call through the client discovers first, with up to three requests, and
// then takes its own request.
func clientFits(requestTimeout, discovery time.Duration) error {
	if requestTimeout <= apiServerRequestTimeout {
		return fmt.Errorf("the client's request timeout %s is not above the %s that kube-apiserver gives a request: it could cut a call that the server would answer", requestTimeout, apiServerRequestTimeout)
	}
	if err := requestFits("the steps' discovery", discovery); err != nil {
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

// The workers go first and a docker that hangs uses their whole bound: what
// remains of the dump has to hold the tenant's bound and as much again for the
// kind cluster's lists.
func tenantFits(requestTimeout, workerBound, dumpBound, dump time.Duration) error {
	if err := requestFits("the tenant's", requestTimeout); err != nil {
		return err
	}
	if workerBound <= 0 || dumpBound <= 0 || workerBound+2*dumpBound >= dump {
		return fmt.Errorf("the workers' part of the dump %s or the tenant's %s is not positive, or the workers' and two of the tenant's are not less than the dump's %s: a docker that hangs would leave the kind cluster and the tenant too little time", workerBound, dumpBound, dump)
	}
	return nil
}

func TestTheBoundsFitUnderTheTimeouts(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	found := goTestTimeout.FindSubmatch(makefile)
	if found == nil {
		t.Fatal("the Makefile runs go test with no -timeout")
	}
	n, err := strconv.Atoi(string(found[1]))
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
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		t.Fatal(err)
	}
	limit := ci.Jobs["e2e"].TimeoutMinutes
	if limit == 0 {
		t.Fatal("the e2e job has no timeout-minutes")
	}
	if err := fits(total(bounds), time.Duration(n)*time.Minute, time.Duration(limit)*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := tenantFits(tenantRequestTimeout, workerDumpBound, tenantDumpBound, bounds.Dump); err != nil {
		t.Fatal(err)
	}
	if err := requestFits("the dump's", dumpRequestTimeout); err != nil {
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
}

func TestTotalAddsEveryBound(t *testing.T) {
	var b boundSet
	var want time.Duration
	fields := reflect.ValueOf(&b).Elem()
	for i := range fields.NumField() {
		d := time.Duration(1<<i) * time.Minute
		fields.Field(i).SetInt(int64(d))
		want += d
	}
	if got := total(b); got != want {
		t.Fatalf("total is %s, the bounds add up to %s", got, want)
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

func TestTenantTimesThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name                                         string
		requestTimeout, workerBound, dumpBound, dump time.Duration
		want                                         string
	}{
		{"a request timeout of 40 seconds against 120", 40 * second, 30 * second, 30 * second, 120 * second, ""},
		{"a request timeout of 41 seconds against 120", 41 * second, 30 * second, 30 * second, 120 * second, "request timeout"},
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
