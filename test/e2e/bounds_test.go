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

// bounds holds every wait of the suite. A step's own bound has to fail before
// go test gives up, since only the step's failure writes the artifacts.
var bounds = struct {
	Install, Dump                                                                                time.Duration
	Apply, Initialized, Endpoint, Kindnet, Running, Ready, Konnectivity, Pod, Available, Deleted time.Duration
}{
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

var goTestTimeout = regexp.MustCompile(`-timeout (\d+)m\b`)

func total() time.Duration {
	var sum time.Duration
	for _, bound := range reflect.ValueOf(bounds).Fields() {
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
	if err := fits(total(), time.Duration(n)*time.Minute, time.Duration(limit)*time.Minute); err != nil {
		t.Fatal(err)
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
