// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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
		controlPlaneKind,
		etcdKind,
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
		{appsv1.SchemeGroupVersion.WithKind("Deployment"), metav1.NamespaceDefault},
		{appsv1.SchemeGroupVersion.WithKind("ReplicaSet"), metav1.NamespaceDefault},
	}
)

// Besides objectKinds, the graph of the quick start checks holds Secrets and
// ConfigMaps, and the CRDs' clusterctl label says which kinds it holds. They
// are dumped without data, and without annotations, which can hold a copy of
// the data.
var metadataKinds = []struct {
	kind      schema.GroupVersionKind
	namespace string
}{
	{corev1.SchemeGroupVersion.WithKind("Secret"), namespace},
	{corev1.SchemeGroupVersion.WithKind("ConfigMap"), namespace},
	{apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"), ""},
}

func dumpRESTConfig(cfg *envconf.Config) *rest.Config {
	restCfg := rest.CopyConfig(cfg.Client().RESTConfig())
	restCfg.Timeout = dumpRequestTimeout
	return restCfg
}

func dumpCluster(ctx context.Context, cfg *envconf.Config, dir string) error {
	c, err := client.New(dumpRESTConfig(cfg), client.Options{})
	if err != nil {
		return err
	}
	return dumpBounded(ctx, cfg, c, dir)
}

func dumpBounded(ctx context.Context, cfg *envconf.Config, c client.Client, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, bounds.Dump)
	defer cancel()
	return dumpAll(ctx, cfg, c, dir, tenantDumpBound, workerDumpBound)
}

func dumpAll(ctx context.Context, cfg *envconf.Config, c client.Client, dir string, tenantBound, workerBound time.Duration) error {
	// Docker does not need the API server, which when it does not answer uses
	// all that remains of the dump.
	errs := []error{dumpWorkers(ctx, cfg, dir, workerBound)}
	for _, kind := range objectKinds {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, dumpObjects(ctx, c, dir, kind, namespace))
	}
	if ctx.Err() == nil {
		errs = append(errs, dumpObjects(ctx, c, dir, datastoreClass, ""))
	}
	for _, m := range metadataKinds {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, dumpMetadata(ctx, c, dir, m.kind, m.namespace))
	}
	for _, ns := range managers {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, dumpObjects(ctx, c, dir, podKind, ns), dumpObjects(ctx, c, dir, eventKind, ns))
	}
	if ctx.Err() == nil {
		errs = append(errs, dumpTenant(ctx, cfg, c, dir, tenantBound))
	}
	// A kubelet that stalls its log requests holds each of them to the request
	// timeout, in every container: the logs go last.
	for _, ns := range managers {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, dumpLogs(ctx, cfg, dir, ns))
	}
	return errors.Join(append(errs, ctx.Err())...)
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

func dumpMetadata(ctx context.Context, c client.Client, dir string, kind schema.GroupVersionKind, ns string) error {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range list.Items {
		list.Items[i].Annotations = nil
		list.Items[i].ManagedFields = nil
	}
	data, err := yaml.Marshal(list)
	if err != nil {
		return err
	}
	return write(filepath.Join(dir, "objects", ns, kind.Kind+".yaml"), data)
}

func dumpLogs(ctx context.Context, cfg *envconf.Config, dir, ns string) error {
	clientset, err := kubernetes.NewForConfig(dumpRESTConfig(cfg))
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
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		errs = append(errs, dumpObjects(ctx, tenant, filepath.Join(dir, "tenant"), o.kind, o.namespace))
	}
	return errors.Join(errs...)
}

func dumpWorkers(ctx context.Context, cfg *envconf.Config, dir string, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
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

func dockerThatHangsOn(verb string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in
%s) exec sleep 10 ;;
ps) echo worker ;;
exec) shift; echo "$@" ;;
esac
`, verb)
}

var tenantKinds = map[string]string{"nodes": "Node", "pods": "Pod", "daemonsets": "DaemonSet", "events": "Event", "deployments": "Deployment", "replicasets": "ReplicaSet"}

func tenantAPI(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(tenantHandler(t))
	t.Cleanup(server.Close)
	return server
}

func tenantHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			respond(t, w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "/apis":
			respond(t, w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}}]}`)
		case "/api/v1":
			respond(t, w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"nodes","kind":"Node","namespaced":false,"verbs":["list"]},{"name":"pods","kind":"Pod","namespaced":true,"verbs":["list"]},{"name":"events","kind":"Event","namespaced":true,"verbs":["list"]}]}`)
		case "/apis/apps/v1":
			respond(t, w, `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[{"name":"daemonsets","kind":"DaemonSet","namespaced":true,"verbs":["list"]},{"name":"deployments","kind":"Deployment","namespaced":true,"verbs":["list"]},{"name":"replicasets","kind":"ReplicaSet","namespaced":true,"verbs":["list"]}]}`)
		default:
			groupVersion := "v1"
			if strings.HasPrefix(r.URL.Path, "/apis/apps/v1/") {
				groupVersion = "apps/v1"
			}
			respond(t, w, fmt.Sprintf(`{"kind":"%sList","apiVersion":%q,"items":[{"metadata":{"name":%q}}]}`, tenantKinds[path.Base(r.URL.Path)], groupVersion, r.URL.Path))
		}
	})
}

func respond(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		t.Error(err)
	}
}

func managementHandler(t *testing.T) http.Handler {
	t.Helper()
	tenant := tenantHandler(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/apis":
			respond(t, w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"cluster.x-k8s.io","versions":[{"groupVersion":"cluster.x-k8s.io/v1beta2","version":"v1beta2"}],"preferredVersion":{"groupVersion":"cluster.x-k8s.io/v1beta2","version":"v1beta2"}}]}`)
		case r.URL.Path == "/apis/cluster.x-k8s.io/v1beta2":
			respond(t, w, `{"kind":"APIResourceList","groupVersion":"cluster.x-k8s.io/v1beta2","resources":[{"name":"clusters","kind":"Cluster","namespaced":true,"verbs":["list"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/clusters"):
			respond(t, w, `{"kind":"ClusterList","apiVersion":"cluster.x-k8s.io/v1beta2","items":[]}`)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			respond(t, w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"q"},"spec":{"containers":[{"name":"d"},{"name":"e"}]},"status":{"containerStatuses":[{"name":"d"},{"name":"e"}]}},{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c"},{"name":"c2"}]},"status":{"containerStatuses":[{"name":"c","restartCount":1},{"name":"c2","restartCount":0}]}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pods/q/log") && r.URL.Query().Get("container") == "d":
			http.Error(w, "container d is waiting to start: ContainerCreating", http.StatusBadRequest)
		case strings.HasSuffix(r.URL.Path, "/log"):
			query := r.URL.Query()
			query.Del("timeout")
			respond(t, w, "log of "+r.URL.Path+"?"+query.Encode())
		default:
			tenant.ServeHTTP(w, r)
		}
	})
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

func configFor(t *testing.T, server string) *envconf.Config {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, serverKubeconfig(t, server, "kind-capl-e2e"), 0o600); err != nil {
		t.Fatal(err)
	}
	return envconf.NewWithKubeConfig(kubeconfig)
}

var dumpScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	return s
}()

type dumpSetup struct {
	cfg *envconf.Config
	c   client.Client
	dir string
}

func useDocker(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func withoutDocker(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func objectOf(kind schema.GroupVersionKind, ns string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(kind)
	o.SetNamespace(ns)
	o.SetName("object-of-" + kind.Kind)
	return o
}

func newDumpSetup(t *testing.T, tenant string) dumpSetup {
	t.Helper()
	useDocker(t, fakeDocker)
	management := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(t, w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
	}))
	t.Cleanup(management.Close)
	builder := fake.NewClientBuilder().WithScheme(dumpScheme)
	for _, ns := range managers {
		builder = builder.WithObjects(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "event-in-" + ns}})
	}
	for _, kind := range objectKinds {
		builder = builder.WithObjects(objectOf(kind, namespace))
	}
	builder = builder.WithObjects(objectOf(datastoreClass, ""))
	labelled := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
		Name: "machines.cluster.x-k8s.io", Labels: map[string]string{"clusterctl.cluster.x-k8s.io": ""}, Annotations: map[string]string{"an-annotation": "annotation-value"},
	}}
	builder = builder.WithObjects(labelled, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "a-config", Annotations: map[string]string{"an-annotation": "annotation-value"}, Finalizers: []string{"a-finalizer"}},
		Data:       map[string]string{"a-key": "config-value"},
	})
	if tenant != "" {
		builder = builder.WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: secret.Name("capl-e2e-w", secret.Kubeconfig)},
			Data:       map[string][]byte{secret.KubeconfigDataName: serverKubeconfig(t, tenant, "tenant")},
		})
	}
	return dumpSetup{cfg: configFor(t, management.URL), c: builder.Build(), dir: t.TempDir()}
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

func requireOnlyFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := make(map[string]string)
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		got[name] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range want {
		if have, ok := got[name]; !ok {
			t.Errorf("%s is not written", name)
		} else if have != content {
			t.Errorf("%s holds %q, want %q", name, have, content)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is written, and should not be", name)
		}
	}
}

var workerFiles = map[string]string{
	"worker/worker/journal.log":   "journalctl --no-pager -u kubelet -u containerd",
	"worker/worker/crictl-ps.txt": "crictl ps -a",
}

func TestTheDumpWritesTheTenantTheEventsAndTheWorkers(t *testing.T) {
	d := newDumpSetup(t, tenantAPI(t).URL)
	if err := dumpAll(t.Context(), d.cfg, d.c, d.dir, time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"tenant/objects/Node.yaml":                  "/api/v1/nodes",
		"tenant/objects/kube-system/Pod.yaml":       "/api/v1/namespaces/kube-system/pods",
		"tenant/objects/kube-system/Event.yaml":     "/api/v1/namespaces/kube-system/events",
		"tenant/objects/kube-system/DaemonSet.yaml": "/apis/apps/v1/namespaces/kube-system/daemonsets",
		"tenant/objects/default/Pod.yaml":           "/api/v1/namespaces/default/pods",
		"tenant/objects/default/Event.yaml":         "/api/v1/namespaces/default/events",
		"tenant/objects/default/Deployment.yaml":    "/apis/apps/v1/namespaces/default/deployments",
		"tenant/objects/default/ReplicaSet.yaml":    "/apis/apps/v1/namespaces/default/replicasets",
		"worker/worker/journal.log":                 "journalctl --no-pager -u kubelet -u containerd",
		"worker/worker/crictl-ps.txt":               "crictl ps -a",
	}
	for _, ns := range managers {
		want["objects/"+ns+"/Event.yaml"] = "event-in-" + ns
	}
	for _, kind := range objectKinds {
		want["objects/"+namespace+"/"+kind.Kind+".yaml"] = "object-of-" + kind.Kind
	}
	want["objects/DatastoreClass.yaml"] = "object-of-DatastoreClass"
	requireFiles(t, d.dir, want)
}

func TestTheDumpOfSecretsConfigMapsAndCRDsHoldsTheirMetadataAlone(t *testing.T) {
	tenant := tenantAPI(t)
	d := newDumpSetup(t, tenant.URL)
	if err := dumpAll(t.Context(), d.cfg, d.c, d.dir, time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	requireFiles(t, d.dir, map[string]string{
		"objects/capi-join/Secret.yaml":         secret.Name("capl-e2e-w", secret.Kubeconfig),
		"objects/capi-join/ConfigMap.yaml":      "a-finalizer",
		"objects/CustomResourceDefinition.yaml": "clusterctl.cluster.x-k8s.io",
	})
	for _, file := range []string{"objects/capi-join/Secret.yaml", "objects/capi-join/ConfigMap.yaml", "objects/CustomResourceDefinition.yaml"} {
		got, err := os.ReadFile(filepath.Join(d.dir, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{tenant.URL, "config-value", "an-annotation", "annotation-value"} {
			if strings.Contains(string(got), leaked) {
				t.Errorf("%s holds %q:\n%s", file, leaked, got)
			}
		}
	}
}

func TestTheDumpOfAnInstallThatFailedBeforeTheTenantHadAKubeconfig(t *testing.T) {
	d := newDumpSetup(t, "")
	if err := dumpAll(t.Context(), d.cfg, d.c, d.dir, time.Minute, time.Minute); err != nil {
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

func TestTheDumpRunsNoDockerOnceItsBoundHasEnded(t *testing.T) {
	d := newDumpSetup(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := dumpBounded(ctx, d.cfg, d.c, d.dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the context's error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "worker")); !os.IsNotExist(err) {
		t.Fatalf("the dump wrote the workers after its bound: %v", err)
	}
}
