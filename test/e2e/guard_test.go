// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
		{"a shorter prefix", kubeconfig(t, kind, "kind-capl-dev", "kind-capl-dev"), "does not start with"},
		{"the prefix inside the name", kubeconfig(t, kind, "x-kind-capl-e2e", "x-kind-capl-e2e"), "does not start with"},
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
	for _, name := range []string{"capl-e2e", "capl-e2e-2", "capl-dev", "lodia-e2e-local", "lodia-e2e-capi"} {
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

func TestMakeAcceptsOnlyNamesWhoseClusterLodiaAccepts(t *testing.T) {
	lodiaName := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	cases := []struct {
		name, want string
	}{
		{"capl-e2e", ""},
		{"capl-e2e-2", ""},
		{"capl-e2ex", ""},
		{"capl-e2e-", ""},
		{"capl-e2e-" + strings.Repeat("x", 21), ""},
		{"capl-e2e-" + strings.Repeat("x", 22), "more than 30 characters"},
		{"capl-e2e-w", "must not end in -w"},
		{"capl-e2e-W", "character outside"},
		{"capl-e2e_x", "character outside"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := exec.Command("make", "--no-print-directory", "-C", "../..", "e2e-cluster", "E2E_CLUSTER="+c.name).CombinedOutput()
			if c.want != "" {
				if err == nil || !strings.Contains(string(out), c.want) {
					t.Fatalf("want make to refuse with %q, got %v\n%s", c.want, err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("make refuses: %v\n%s", err, out)
			}
			if cluster := c.name + "-w"; len(cluster) > 32 || !lodiaName.MatchString(cluster) {
				t.Fatalf("make accepts %s, whose Cluster %s Lodia refuses", c.name, cluster)
			}
		})
	}
}

func requireClustersOfTheirOwn(clusters map[string]string) error {
	made := make(map[string]string)
	for _, target := range slices.Sorted(maps.Keys(clusters)) {
		name := clusters[target]
		if other, ok := made[name]; ok {
			return fmt.Errorf("the targets %s and %s both create the cluster %s: two jobs on one host would share its directory and its kubeconfig", other, target, name)
		}
		made[name] = target
	}
	return nil
}

func TestNoTwoTargetsShareACluster(t *testing.T) {
	cases := []struct {
		name     string
		clusters map[string]string
		want     string
	}{
		{"no target", nil, ""},
		{"two targets with a cluster each", map[string]string{"e2e": "capl-e2e", "e2e-upgrade": "capl-e2e-upgrade"}, ""},
		{"three targets with a cluster each", map[string]string{"e2e": "capl-e2e", "e2e-upgrade": "capl-e2e-upgrade", "e2e-minor": "capl-e2e-minor"}, ""},
		{"two targets with one cluster", map[string]string{"e2e": "capl-e2e", "e2e-upgrade": "capl-e2e"}, "the targets e2e and e2e-upgrade both create the cluster capl-e2e"},
		{"the third target with the cluster of the first", map[string]string{"e2e": "capl-e2e", "e2e-upgrade": "capl-e2e-upgrade", "e2e-minor": "capl-e2e"}, "the targets e2e and e2e-minor both create the cluster capl-e2e"},
		{"the third target with the cluster of the second", map[string]string{"e2e": "capl-e2e", "e2e-upgrade": "capl-e2e-upgrade", "e2e-minor": "capl-e2e-upgrade"}, "the targets e2e-minor and e2e-upgrade both create the cluster capl-e2e-upgrade"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireClustersOfTheirOwn(c.clusters)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestEachTargetThatRunsTheSuiteCreatesAClusterOfItsOwnThatTheGuardAccepts(t *testing.T) {
	creates := regexp.MustCompile(`kind create cluster --name (\S+) --kubeconfig (\S+)`)
	runs := regexp.MustCompile(`go test [^;&|\n]*-tags e2e`)
	clusters := make(map[string]string)
	for target, tags := range map[string]string{"e2e": "-tags e2e ", "e2e-upgrade": "-tags e2e,upgrade "} {
		t.Run(target, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "make", "--no-print-directory", target)
			cmd.Dir = root
			recipe := recipeOf(t, cmd)
			found := creates.FindStringSubmatch(recipe)
			if found == nil {
				t.Fatalf("the recipe creates no kind cluster:\n%s", recipe)
			}
			name, kubeconfigPath := found[1], found[2]
			clusters[target] = name
			if n := len(runs.FindAllString(recipe, -1)); n != 1 {
				t.Errorf("the target runs go test with -tags e2e %d times, want once, since a suite that a prerequisite or a recursive make runs besides it is on the same cluster and under no bound of the target:\n%s", n, recipe)
			}
			if err := RequireNamedCluster(kubeconfig(t, "https://127.0.0.1:6443", "kind-"+name, "kind-"+name)); err != nil {
				t.Errorf("the guard refuses the cluster %s: %v", name, err)
			}
			if out, err := exec.CommandContext(t.Context(), "make", "--no-print-directory", "-C", root, "e2e-cluster", "E2E_CLUSTER="+name).CombinedOutput(); err != nil {
				t.Errorf("make e2e-cluster refuses %s: %v\n%s", name, err, out)
			}
			for _, want := range []string{
				"E2E_KUBECONFIG=" + kubeconfigPath + " go test -v " + tags,
				"label=io.x-k8s.kind.cluster=" + name + "-w",
				"kind delete cluster --name " + name + " --kubeconfig " + kubeconfigPath,
			} {
				if !strings.Contains(recipe, want) {
					t.Errorf("the recipe lacks %q:\n%s", want, recipe)
				}
			}
			if !strings.HasSuffix(filepath.Dir(kubeconfigPath), filepath.Join("work", "e2e", name)) {
				t.Errorf("the kubeconfig %s is not in work/e2e/%s", kubeconfigPath, name)
			}
		})
	}
	if err := requireClustersOfTheirOwn(clusters); err != nil {
		t.Error(err)
	}
}
