// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

type closedNetwork struct{}

func (closedNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the test closed the network")
}

func TestTheCommandsRefuse(t *testing.T) {
	saved := http.DefaultTransport
	http.DefaultTransport = closedNetwork{}
	t.Cleanup(func() { http.DefaultTransport = saved })
	t.Setenv("GITHUB_TOKEN", "not-a-token")
	r := newRepository(t)
	r.git("remote", "add", "origin", "https://github.com/lodia-io/cluster-api-provider-lodia.git")
	older := r.commit(series("0.1.v1beta2"))
	r.originMain(r.commit(series("0.1.v1beta2", "0.2.v1beta2")))
	r.write(crdsDir+"/0.yaml", crd("cluster.x-k8s.io/v1beta2"))
	r.write(metadataFile, series("0.2.v1beta2"))
	t.Chdir(r.dir)

	if err := run(t.Context(), []string{"metadata"}); err == nil || !strings.Contains(err.Error(), "has the series 0.1") {
		t.Errorf("metadata: want the dropped series refused, got %v", err)
	}
	if err := run(t.Context(), []string{"tag", "--commit=" + older, "v0.2.0"}); err == nil || !strings.Contains(err.Error(), "no release series 0.2 in metadata.yaml at "+older) {
		t.Errorf("tag --commit: want the older commit refused, got %v", err)
	}
}
