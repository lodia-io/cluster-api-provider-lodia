// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/go-github/v92/github"
)

const (
	ciWorkflow = "ci.yml"
	ciEvent    = "push"
	ciBranch   = "main"
)

var origin = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

type githubCI struct {
	client      *github.Client
	owner, repo string
}

func newGitHub(owner, repo, token string, opts ...github.ClientOptionsFunc) (*githubCI, error) {
	client, err := github.NewClient(append([]github.ClientOptionsFunc{github.WithAuthToken(token)}, opts...)...)
	if err != nil {
		return nil, err
	}
	return &githubCI{client: client, owner: owner, repo: repo}, nil
}

func originRepository(ctx context.Context, dir string) (owner, repo string, err error) {
	out, err := git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	url := strings.TrimSpace(string(out))
	m := origin.FindStringSubmatch(url)
	if m == nil {
		return "", "", fmt.Errorf("origin %q is not a github.com repository", url)
	}
	return m[1], m[2], nil
}

func (g *githubCI) Runs(ctx context.Context, sha string) ([]Run, error) {
	list, _, err := g.client.Actions.ListWorkflowRunsByFileName(ctx, g.owner, g.repo, ciWorkflow, &github.ListWorkflowRunsOptions{
		Branch:      ciBranch,
		Event:       ciEvent,
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		if refused, ok := errors.AsType[*github.ErrorResponse](err); ok && refused.Response != nil &&
			(refused.Response.StatusCode == http.StatusForbidden || refused.Response.StatusCode == http.StatusNotFound) {
			return nil, fmt.Errorf("listing the runs of %s answered %d: the token needs actions: read on %s/%s: %w", ciWorkflow, refused.Response.StatusCode, g.owner, g.repo, err)
		}
		return nil, fmt.Errorf("listing the runs of %s: %w", ciWorkflow, err)
	}
	runs := make([]Run, 0, len(list.WorkflowRuns))
	for _, run := range list.WorkflowRuns {
		runs = append(runs, Run{URL: run.GetHTMLURL(), Status: run.GetStatus(), Conclusion: run.GetConclusion()})
	}
	return runs, nil
}
