// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func kubeconfig(t *testing.T, server, current string, contexts ...string) string {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	if server != "" {
		cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: server}
	}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "t"}
	for _, name := range contexts {
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	}
	cfg.CurrentContext = current
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequireNamedCluster(t *testing.T) {
	const kind = "https://127.0.0.1:6443"
	cases := []struct {
		name, path, want string
	}{
		{"no kubeconfig", "", "E2E_KUBECONFIG is not set"},
		{"missing file", filepath.Join(t.TempDir(), "absent"), "read "},
		{"two contexts", kubeconfig(t, kind, "kind-capl-e2e", "kind-capl-e2e", "segla-wg"), "needs exactly one"},
		{"someone's cluster", kubeconfig(t, kind, "segla-wg", "segla-wg"), "does not start with"},
		{"another kind cluster", kubeconfig(t, kind, "kind-lodia-hardway", "kind-lodia-hardway"), "does not start with"},
		{"Lodia's e2e cluster", kubeconfig(t, kind, "kind-lodia-e2e-local", "kind-lodia-e2e-local"), "does not start with"},
		{"current context absent", kubeconfig(t, kind, "kind-capl-e2e-x", "kind-capl-e2e-y"), "does not exist"},
		{"a context without its cluster", kubeconfig(t, "", "kind-capl-e2e", "kind-capl-e2e"), "does not hold"},
		{"the name on another host", kubeconfig(t, "https://segla.example.io:6443", "kind-capl-e2e", "kind-capl-e2e"), "not a loopback address"},
		{"the name on a host that starts like an address", kubeconfig(t, "https://127.0.0.1.example.io:6443", "kind-capl-e2e", "kind-capl-e2e"), "not a loopback address"},
		{"the name on a private address", kubeconfig(t, "https://10.0.0.1:6443", "kind-capl-e2e", "kind-capl-e2e"), "not a loopback address"},
		{"the e2e cluster", kubeconfig(t, kind, "kind-capl-e2e", "kind-capl-e2e"), ""},
		{"an e2e cluster of another name", kubeconfig(t, kind, "kind-capl-e2e-2", "kind-capl-e2e-2"), ""},
		{"an e2e cluster on IPv6", kubeconfig(t, "https://[::1]:6443", "kind-capl-e2e", "kind-capl-e2e"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := RequireNamedCluster(c.path)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestMakeAndTheGuardAgreeOnTheClusterName(t *testing.T) {
	for _, name := range []string{"capl-e2e", "capl-e2e-2", "lodia-e2e-local", "lodia-e2e-capi"} {
		t.Run(name, func(t *testing.T) {
			_, err := exec.Command("make", "--no-print-directory", "-C", "../..", "e2e-cluster", "E2E_CLUSTER="+name).CombinedOutput()
			guard := RequireNamedCluster(kubeconfig(t, "https://127.0.0.1:6443", "kind-"+name, "kind-"+name))
			if (err == nil) != (guard == nil) {
				t.Fatalf("make e2e-cluster says %v, the guard says %v", err, guard)
			}
			if wantAccepted := strings.HasPrefix(name, "capl-e2e"); wantAccepted != (guard == nil) {
				t.Fatalf("the guard says %v about %s", guard, name)
			}
		})
	}
}
