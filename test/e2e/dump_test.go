// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"
)

var (
	objectKinds = []schema.GroupVersionKind{
		{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"},
		{Group: "controlplane.lodia.io", Version: "v1alpha1", Kind: "LodiaControlPlane"},
		devCluster,
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DevMachineTemplate"},
		{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta2", Kind: "KubeadmConfigTemplate"},
		{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineDeployment"},
		{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineSet"},
		{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"},
		devMachine,
		{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta2", Kind: "KubeadmConfig"},
		{Group: "lodia.io", Version: "v1alpha1", Kind: "ControlPlane"},
		{Group: "druid.gardener.cloud", Version: "v1alpha1", Kind: "Etcd"},
	}
	datastoreClass = schema.GroupVersionKind{Group: "lodia.io", Version: "v1alpha1", Kind: "DatastoreClass"}
	managers       = []string{"cert-manager", "capi-system", "capi-kubeadm-bootstrap-system", "capd-system", providerNamespace, lodiaNamespace, namespace}
	podKind        = corev1.SchemeGroupVersion.WithKind("Pod")
	eventKind      = corev1.SchemeGroupVersion.WithKind("Event")
	tenantObjects  = []struct {
		kind      schema.GroupVersionKind
		namespace string
	}{
		{corev1.SchemeGroupVersion.WithKind("Node"), ""},
		{podKind, metav1.NamespaceSystem},
		{podKind, metav1.NamespaceDefault},
		{appsv1.SchemeGroupVersion.WithKind("DaemonSet"), metav1.NamespaceSystem},
		{eventKind, metav1.NamespaceSystem},
		{eventKind, metav1.NamespaceDefault},
	}
)

func dumpAll(ctx context.Context, cfg *envconf.Config, c client.Client, dir string, tenantBound time.Duration) error {
	var errs []error
	for _, kind := range objectKinds {
		errs = append(errs, dumpObjects(ctx, c, dir, kind, namespace))
	}
	errs = append(errs, dumpObjects(ctx, c, dir, datastoreClass, ""))
	for _, ns := range managers {
		errs = append(errs, dumpObjects(ctx, c, dir, podKind, ns), dumpObjects(ctx, c, dir, eventKind, ns), dumpLogs(ctx, cfg, dir, ns))
	}
	errs = append(errs, dumpTenant(ctx, cfg, c, dir, tenantBound), dumpWorkers(ctx, cfg, dir))
	return errors.Join(errs...)
}

func dumpObjects(ctx context.Context, c client.Client, dir string, kind schema.GroupVersionKind, ns string) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return err
	}
	data, err := yaml.Marshal(list.UnstructuredContent())
	if err != nil {
		return err
	}
	return write(filepath.Join(dir, "objects", ns, kind.Kind+".yaml"), data)
}

func dumpLogs(ctx context.Context, cfg *envconf.Config, dir, ns string) error {
	clientset, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		return err
	}
	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var errs []error
	for _, pod := range pods.Items {
		restarts := make(map[string]int32)
		for _, status := range pod.Status.ContainerStatuses {
			restarts[status.Name] = status.RestartCount
		}
		for _, container := range pod.Spec.Containers {
			for _, previous := range []bool{false, true} {
				if previous && restarts[container.Name] == 0 {
					continue
				}
				logs, err := clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name, Previous: previous}).DoRaw(ctx)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				name := pod.Name + "-" + container.Name
				if previous {
					name += ".previous"
				}
				errs = append(errs, write(filepath.Join(dir, "logs", ns, name+".log"), logs))
			}
		}
	}
	return errors.Join(errs...)
}

func dumpTenant(ctx context.Context, cfg *envconf.Config, c client.Client, dir string, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	cluster, err := clusterName(cfg)
	if err != nil {
		return err
	}
	restCfg, err := tenantRESTConfig(ctx, c, client.ObjectKey{Namespace: namespace, Name: cluster}, bound/time.Duration(len(tenantObjects)))
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	tenant, err := client.New(restCfg, client.Options{})
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range tenantObjects {
		errs = append(errs, dumpObjects(ctx, tenant, filepath.Join(dir, "tenant"), o.kind, o.namespace))
	}
	return errors.Join(errs...)
}

func dumpWorkers(ctx context.Context, cfg *envconf.Config, dir string) error {
	cluster, err := clusterName(cfg)
	if err != nil {
		return err
	}
	containers, err := docker(ctx, "ps", "--all", "--quiet", "--filter", "label="+clusterLabel+"="+cluster)
	if err != nil {
		return err
	}
	var errs []error
	for container := range strings.FieldsSeq(containers) {
		for name, command := range map[string][]string{
			"journal.log":   {"journalctl", "--no-pager", "-u", "kubelet", "-u", "containerd"},
			"crictl-ps.txt": {"crictl", "ps", "-a"},
		} {
			out, err := docker(ctx, append([]string{"exec", container}, command...)...)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			errs = append(errs, write(filepath.Join(dir, "worker", container, name), []byte(out)))
		}
	}
	return errors.Join(errs...)
}

func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

const fakeDocker = `#!/bin/sh
case "$1" in
ps) echo worker ;;
exec) shift; echo "$@" ;;
esac
`

var tenantKinds = map[string]string{"nodes": "Node", "pods": "Pod", "daemonsets": "DaemonSet", "events": "Event"}

func tenantAPI(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			respond(t, w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "/apis":
			respond(t, w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}}]}`)
		case "/api/v1":
			respond(t, w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"nodes","kind":"Node","namespaced":false,"verbs":["list"]},{"name":"pods","kind":"Pod","namespaced":true,"verbs":["list"]},{"name":"events","kind":"Event","namespaced":true,"verbs":["list"]}]}`)
		case "/apis/apps/v1":
			respond(t, w, `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[{"name":"daemonsets","kind":"DaemonSet","namespaced":true,"verbs":["list"]}]}`)
		default:
			groupVersion := "v1"
			if strings.HasPrefix(r.URL.Path, "/apis/apps/v1/") {
				groupVersion = "apps/v1"
			}
			respond(t, w, fmt.Sprintf(`{"kind":"%sList","apiVersion":%q,"items":[{"metadata":{"name":%q}}]}`, tenantKinds[path.Base(r.URL.Path)], groupVersion, r.URL.Path))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func respond(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		t.Error(err)
	}
}

func serverKubeconfig(t *testing.T, server, context string) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: server, InsecureSkipTLSVerify: true}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts[context] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	cfg.CurrentContext = context
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type dumpSetup struct {
	cfg *envconf.Config
	c   client.Client
	dir string
}

func newDumpSetup(t *testing.T, tenant string) dumpSetup {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	management := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(t, w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
	}))
	t.Cleanup(management.Close)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, serverKubeconfig(t, management.URL, "kind-capl-e2e"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder()
	for _, ns := range managers {
		builder = builder.WithObjects(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "event-in-" + ns}})
	}
	if tenant != "" {
		builder = builder.WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: secret.Name("capl-e2e-w", secret.Kubeconfig)},
			Data:       map[string][]byte{secret.KubeconfigDataName: serverKubeconfig(t, tenant, "tenant")},
		})
	}
	return dumpSetup{cfg: envconf.NewWithKubeConfig(kubeconfig), c: builder.Build(), dir: t.TempDir()}
}

func requireFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	for file, content := range want {
		got, err := os.ReadFile(filepath.Join(dir, file))
		switch {
		case err != nil:
			t.Errorf("%s: %v", file, err)
		case !strings.Contains(string(got), content):
			t.Errorf("%s lacks %q:\n%s", file, content, got)
		}
	}
}

func TestTheDumpWritesTheTenantTheEventsAndTheWorkers(t *testing.T) {
	d := newDumpSetup(t, tenantAPI(t).URL)
	if err := dumpAll(t.Context(), d.cfg, d.c, d.dir, time.Minute); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"tenant/objects/Node.yaml":                  "/api/v1/nodes",
		"tenant/objects/kube-system/Pod.yaml":       "/api/v1/namespaces/kube-system/pods",
		"tenant/objects/kube-system/Event.yaml":     "/api/v1/namespaces/kube-system/events",
		"tenant/objects/kube-system/DaemonSet.yaml": "/apis/apps/v1/namespaces/kube-system/daemonsets",
		"tenant/objects/default/Pod.yaml":           "/api/v1/namespaces/default/pods",
		"tenant/objects/default/Event.yaml":         "/api/v1/namespaces/default/events",
		"worker/worker/journal.log":                 "journalctl --no-pager -u kubelet -u containerd",
		"worker/worker/crictl-ps.txt":               "crictl ps -a",
	}
	for _, ns := range managers {
		want["objects/"+ns+"/Event.yaml"] = "event-in-" + ns
	}
	requireFiles(t, d.dir, want)
}

func TestTheDumpOfAnInstallThatFailedBeforeTheTenantHadAKubeconfig(t *testing.T) {
	d := newDumpSetup(t, "")
	if err := dumpAll(t.Context(), d.cfg, d.c, d.dir, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "tenant")); !os.IsNotExist(err) {
		t.Fatalf("the dump wrote a tenant directory without a kubeconfig: %v", err)
	}
	requireFiles(t, d.dir, map[string]string{
		"objects/" + providerNamespace + "/Event.yaml": "event-in-" + providerNamespace,
		"worker/worker/journal.log":                    "journalctl",
	})
}
