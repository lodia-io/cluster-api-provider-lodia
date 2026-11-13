// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v92/github"
)

func githubAt(t *testing.T, handler http.HandlerFunc) *githubCI {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base := server.URL + "/"
	ci, err := newGitHub("lodia-io", "cluster-api-provider-lodia", "the-token", github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return ci
}

func TestTheRunsAreCIsPushesToMainOfTheCommit(t *testing.T) {
	ci := githubAt(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/repos/lodia-io/cluster-api-provider-lodia/actions/workflows/ci.yml/runs"; r.URL.Path != want {
			t.Errorf("asked for %s, want %s", r.URL.Path, want)
		}
		query := r.URL.Query()
		for key, want := range map[string]string{"branch": "main", "event": "push", "head_sha": "abc123"} {
			if got := query.Get(key); got != want {
				t.Errorf("asked with %s=%q, want %q", key, got, want)
			}
		}
		if got := r.Header.Get("Authorization"); got != "Bearer the-token" {
			t.Errorf("asked with Authorization %q", got)
		}
		_, _ = w.Write([]byte(`{"total_count": 2, "workflow_runs": [
			{"html_url": "https://example.test/runs/1", "status": "completed", "conclusion": "success"},
			{"html_url": "https://example.test/runs/2", "status": "in_progress", "conclusion": null}
		]}`))
	})

	got, err := ci.Runs(t.Context(), "abc123")

	if err != nil {
		t.Fatal(err)
	}
	want := []Run{
		{URL: "https://example.test/runs/1", Status: "completed", Conclusion: "success"},
		{URL: "https://example.test/runs/2", Status: "in_progress"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("runs (-want +got):\n%s", diff)
	}
}

func TestTheRunsNameThePermissionOnA403And404(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		ci := githubAt(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message": "Resource not accessible by integration"}`))
		})

		_, err := ci.Runs(t.Context(), "abc123")

		for _, want := range []string{fmt.Sprintf("answered %d", status), "actions: read"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%d: want an error containing %q, got %v", status, want, err)
			}
		}
	}
}

func TestTheRunsReportAnyOtherFailure(t *testing.T) {
	ci := githubAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := ci.Runs(t.Context(), "abc123")

	if err == nil || !strings.Contains(err.Error(), "listing the runs of ci.yml") || strings.Contains(err.Error(), "actions: read") {
		t.Errorf("want the plain error, got %v", err)
	}
}

func TestTheRepositoryIsOrigins(t *testing.T) {
	for _, url := range []string{
		"git@github.com:lodia-io/cluster-api-provider-lodia.git",
		"https://github.com/lodia-io/cluster-api-provider-lodia",
		"https://github.com/lodia-io/cluster-api-provider-lodia.git",
		"ssh://git@github.com/lodia-io/cluster-api-provider-lodia.git",
		"https://x-access-token:token@github.com/lodia-io/cluster-api-provider-lodia/",
	} {
		r := newRepository(t)
		r.git("remote", "add", "origin", url)

		owner, repo, err := originRepository(t.Context(), r.dir)

		if err != nil || owner != "lodia-io" || repo != "cluster-api-provider-lodia" {
			t.Errorf("%s: got %q %q %v", url, owner, repo, err)
		}
	}

	r := newRepository(t)
	r.git("remote", "add", "origin", "https://gitlab.com/lodia-io/cluster-api-provider-lodia.git")
	if _, _, err := originRepository(t.Context(), r.dir); err == nil {
		t.Error("an origin outside github.com: want a refusal")
	}
}
