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
	Install, Dump                                                                                time.Duration
	Apply, Initialized, Endpoint, Kindnet, Running, Ready, Konnectivity, Pod, Available, Deleted time.Duration
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
	Deleted:      3 * time.Minute,
}

const (
	// callsWithoutBound covers the calls between the waits and what a poll
	// overshoots by.
	callsWithoutBound = 2 * time.Minute
	// outsideTheBinary is what the job spends beyond -timeout: the build before
	// the binary starts, go test's minute of grace, and the removal of the
	// containers and the cluster.
	outsideTheBinary = 8 * time.Minute
)

const tenantRequestTimeout = 30 * time.Second

// The suite's own client has none: a create through one of cert-manager's
// webhooks can take the 30 seconds the API server gives it.
var dumpRequestTimeout = 10 * time.Second

var tenantDumpBound = 30 * time.Second

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

// The tenant's part of the dump ends a discovery, half the bound, after its own.
func tenantFits(requestTimeout, dumpBound, dump time.Duration) error {
	if err := requestFits("the tenant's", requestTimeout); err != nil {
		return err
	}
	if dumpBound <= 0 || 2*dumpBound >= dump {
		return fmt.Errorf("the tenant's part of the dump %s is not positive, or two of them are not less than the dump's %s: a tenant that does not answer would leave the workers no time", dumpBound, dump)
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
	if err := tenantFits(tenantRequestTimeout, tenantDumpBound, bounds.Dump); err != nil {
		t.Fatal(err)
	}
	if err := requestFits("the dump's", dumpRequestTimeout); err != nil {
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
		{"a timeout the job holds", 20 * minute, 42 * minute, 50 * minute, ""},
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

func TestTenantTimesThatDoNotFitAreRefused(t *testing.T) {
	const second = time.Second
	cases := []struct {
		name                            string
		requestTimeout, dumpBound, dump time.Duration
		want                            string
	}{
		{"a request timeout of 40 seconds against 120", 40 * second, 30 * second, 120 * second, ""},
		{"a request timeout of 41 seconds against 120", 41 * second, 30 * second, 120 * second, "request timeout"},
		{"no request timeout", 0, 30 * second, 120 * second, "request timeout"},
		{"a negative request timeout", -second, 30 * second, 120 * second, "request timeout"},
		{"a tenant dump of 59 seconds against 120", 30 * second, 59 * second, 120 * second, ""},
		{"a tenant dump of 60 seconds against 120", 30 * second, 60 * second, 120 * second, "part of the dump"},
		{"no tenant dump", 30 * second, 0, 120 * second, "part of the dump"},
		{"a negative tenant dump", 30 * second, -second, 120 * second, "part of the dump"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := tenantFits(c.requestTimeout, c.dumpBound, c.dump)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
