// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package clusterctlproxy

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

func TestTheTrapIsWhatClientGoAndClusterctlsOwnProxyLoadWhereNoKubeconfigIsNamed(t *testing.T) {
	url, _ := trap(t)
	configClient, err := config.New(t.Context(), "", config.InjectReader(config.NewMemoryReader()))
	if err != nil {
		t.Fatal(err)
	}
	loaders := map[string]func() (*rest.Config, error){}
	for name, current := range map[string]string{"no context": "", "the context of Kubeconfig": Kubeconfig.Context} {
		loaders["client-go's default loading rules with "+name] = func() (*rest.Config, error) {
			return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: current}).ClientConfig()
		}
		loaders["clusterctl's own proxy with "+name] = func() (*rest.Config, error) {
			return cluster.New(cluster.Kubeconfig{Context: current}, configClient).Proxy().GetConfig()
		}
	}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			cfg, err := load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Host != url {
				t.Errorf("it reaches %s, not the trap at %s", cfg.Host, url)
			}
		})
	}
}

type recording struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (r *recording) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recording) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recording) finish() {
	for _, f := range slices.Backward(r.cleanups) {
		f()
	}
}

func TestTheTrapFailsTheTestOnlyWhenItsClusterIsAsked(t *testing.T) {
	for name, asks := range map[string]bool{"a test that asks nothing": false, "a test that reaches the cluster": true} {
		t.Run(name, func(t *testing.T) {
			r := &recording{TB: t}
			TrapTheDefaultKubeconfig(r)
			if asks {
				cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
				if err != nil {
					t.Fatal(err)
				}
				cfg.Timeout = 200 * time.Millisecond
				c, err := rest.HTTPClientFor(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if resp, err := c.Get(cfg.Host + "/version"); err == nil {
					_ = resp.Body.Close()
					t.Fatal("the trap answered")
				}
			}
			r.finish()
			if got := strings.Join(r.errors, "\n"); asks != strings.Contains(got, "reached its cluster") {
				t.Errorf("a test that asks the cluster: %t, the failures it got: %q", asks, got)
			}
		})
	}
}
