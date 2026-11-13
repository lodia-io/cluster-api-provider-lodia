// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package clusterctlproxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// TrapTheDefaultKubeconfig points KUBECONFIG, which clusterctl's own proxy
// loads when a call names no kubeconfig, at a cluster that never answers, and
// fails the test if that cluster is asked anything. The kubeconfig names the
// cluster as its current context and as Kubeconfig's context.
//
// HOME needs no trap: client-go fixes the path of ~/.kube/config when it loads,
// and reads it only where KUBECONFIG is unset:
// https://github.com/kubernetes/client-go/blob/v0.37.2/tools/clientcmd/loader.go#L47-L48
// https://github.com/kubernetes/client-go/blob/v0.37.2/tools/clientcmd/loader.go#L164-L172
func TrapTheDefaultKubeconfig(t *testing.T) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []string
	)
	record := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, what)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		record(r.Method + " " + r.URL.Path)
		<-r.Context().Done()
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			record("a connection")
		}
	}
	server.StartTLS()

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["trap"] = &clientcmdapi.Cluster{Server: server.URL, InsecureSkipTLSVerify: true}
	cfg.AuthInfos["trap"] = &clientcmdapi.AuthInfo{Token: "t"}
	for _, name := range []string{"trap", Kubeconfig.Context} {
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: "trap", AuthInfo: "trap"}
	}
	cfg.CurrentContext = "trap"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)

	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if len(asked) > 0 {
			t.Errorf("clusterctl loaded the kubeconfig that KUBECONFIG names and reached its cluster: %s", strings.Join(asked, ", "))
		}
	})
}
