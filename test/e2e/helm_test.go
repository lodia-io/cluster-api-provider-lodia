// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

func refuseHelmKubeEnv(environ []string) error {
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "HELM_KUBE") {
			return fmt.Errorf("%s is set: helm lays it over the kubeconfig the guard checked", name)
		}
	}
	return nil
}

func TestHelmKubeVariablesAreRefused(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		want    string
	}{
		{"none", []string{"PATH=/bin", "KUBECONFIG=/x", "HELM_NAMESPACE=y"}, ""},
		{"a name that only mentions one in its value", []string{"X=HELM_KUBETOKEN"}, ""},
		{"the API server", []string{"PATH=/bin", "HELM_KUBEAPISERVER=https://example.io"}, "HELM_KUBEAPISERVER"},
		{"the token", []string{"HELM_KUBETOKEN=t"}, "HELM_KUBETOKEN"},
		{"the CA file", []string{"HELM_KUBECAFILE=/ca"}, "HELM_KUBECAFILE"},
		{"the context, set to nothing", []string{"HELM_KUBECONTEXT="}, "HELM_KUBECONTEXT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := refuseHelmKubeEnv(c.environ)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
