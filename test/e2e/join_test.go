// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

const (
	namespace = "capi-join"
	agnhost   = "registry.k8s.io/e2e-test-images/agnhost:2.66.1@sha256:6e2eef87d0dc77e070a7549bc5fdfa290c0fefe3359eb33c2ad1eda692fe66a4"
)

var (
	devCluster = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DevCluster"}
	devMachine = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DevMachine"}
)

func TestAWorkerJoinsAClusterOfLodiasControlPlane(t *testing.T) {
	var (
		k8s     client.Client
		key     client.ObjectKey
		tenant  *tenantAccess
		machine *clusterv1.Machine
		node    *corev1.Node
	)
	f := features.New("a worker joins a cluster of Lodia's control plane").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			name, err := clusterName(cfg)
			if err != nil {
				t.Fatal(err)
			}
			key = client.ObjectKey{Namespace: namespace, Name: name}
			k8s = newClient(t, cfg)
			return ctx
		}).
		Assess("the objects are applied, and the DevCluster, whose infrastructure the suite manages, is marked provisioned", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			bounded, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			for _, file := range []string{"cluster.yaml", "workers.yaml"} {
				manifest, err := render(file, key.Name)
				if err != nil {
					t.Fatal(err)
				}
				if err := apply(bounded, cfg, manifest); err != nil {
					t.Fatalf("apply %s: %v", file, err)
				}
			}
			provisioned := &unstructured.Unstructured{}
			provisioned.SetGroupVersionKind(devCluster)
			provisioned.SetNamespace(key.Namespace)
			provisioned.SetName(key.Name)
			patch := client.RawPatch(types.MergePatchType, []byte(`{"status":{"initialization":{"provisioned":true}}}`))
			if err := k8s.Status().Patch(bounded, provisioned, patch); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the Cluster reports ControlPlaneInitialized", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, 5*time.Minute, "ControlPlaneInitialized is True", func(ctx context.Context) error {
				cluster := &clusterv1.Cluster{}
				if err := k8s.Get(ctx, key, cluster); err != nil {
					return err
				}
				if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition); c == nil || c.Status != metav1.ConditionTrue {
					return fmt.Errorf("ControlPlaneInitialized is %+v", c)
				}
				return nil
			})
			return ctx
		}).
		Assess("the Cluster's endpoint is the one Lodia published", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, 30*time.Second, "Cluster.spec.controlPlaneEndpoint is the ControlPlane's status.endpoint.apiServer", func(ctx context.Context) error {
				cluster, cp := &clusterv1.Cluster{}, &lodiav1alpha1.ControlPlane{}
				if err := k8s.Get(ctx, key, cluster); err != nil {
					return err
				}
				if err := k8s.Get(ctx, key, cp); err != nil {
					return err
				}
				if cp.Status.Endpoint == nil {
					return fmt.Errorf("the ControlPlane has no status.endpoint")
				}
				want := clusterv1.APIEndpoint{Host: cp.Status.Endpoint.APIServer.Host, Port: cp.Status.Endpoint.APIServer.Port}
				if cluster.Spec.ControlPlaneEndpoint != want {
					return fmt.Errorf("the Cluster's endpoint is %+v, the ControlPlane's %+v", cluster.Spec.ControlPlaneEndpoint, want)
				}
				return nil
			})
			return ctx
		}).
		Assess("the provider copied the kubeconfig and the CA's certificate, never its key, and owns both", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			lcp := &v1alpha1.LodiaControlPlane{}
			if err := k8s.Get(ctx, key, lcp); err != nil {
				t.Fatal(err)
			}
			kubeconfig, ca := &corev1.Secret{}, &corev1.Secret{}
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)}, kubeconfig); err != nil {
				t.Fatal(err)
			}
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.ClusterCA)}, ca); err != nil {
				t.Fatal(err)
			}
			if len(kubeconfig.Data[secret.KubeconfigDataName]) == 0 {
				t.Errorf("Secret %s holds no %s", kubeconfig.Name, secret.KubeconfigDataName)
			}
			if len(ca.Data[secret.TLSCrtDataName]) == 0 {
				t.Errorf("Secret %s holds no %s", ca.Name, secret.TLSCrtDataName)
			}
			if _, ok := ca.Data[secret.TLSKeyDataName]; ok {
				t.Errorf("Secret %s holds the CA's key", ca.Name)
			}
			for _, s := range []*corev1.Secret{kubeconfig, ca} {
				if !metav1.IsControlledBy(s, lcp) {
					t.Errorf("Secret %s is not controlled by LodiaControlPlane %s", s.Name, lcp.Name)
				}
			}
			return ctx
		}).
		Assess("kindnet, from the management node's own manifest, is applied in the tenant", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			tenant = openTenant(ctx, t, k8s, key)
			cluster := &clusterv1.Cluster{}
			if err := k8s.Get(ctx, key, cluster); err != nil {
				t.Fatal(err)
			}
			management, err := kindCluster(cfg.KubeconfigFile())
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := docker(ctx, "exec", management+"-control-plane", "cat", "/kind/manifests/default-cni.yaml")
			if err != nil {
				t.Fatal(err)
			}
			cni, err := template.New("cni").Parse(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var rendered bytes.Buffer
			if err := cni.Execute(&rendered, struct{ PodSubnet string }{cluster.Spec.ClusterNetwork.Pods.CIDRBlocks[0]}); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, time.Minute, "kindnet is applied", func(ctx context.Context) error {
				return decoder.DecodeEach(ctx, bytes.NewReader(rendered.Bytes()), ignoreExisting(decoder.CreateHandler(tenant.resources)))
			})
			return ctx
		}).
		Assess("the Machine is Running with a nodeRef, and Lodia reported the version the MachineSet's preflight reads", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, 5*time.Minute, "the Machine is Running with a nodeRef", func(ctx context.Context) error {
				machines := &clusterv1.MachineList{}
				if err := k8s.List(ctx, machines, client.InNamespace(key.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: key.Name}); err != nil {
					return err
				}
				if len(machines.Items) != 1 {
					return fmt.Errorf("%d Machines, want 1", len(machines.Items))
				}
				m := &machines.Items[0]
				if m.Status.Phase != string(clusterv1.MachinePhaseRunning) || m.Status.NodeRef.Name == "" {
					return fmt.Errorf("Machine %s is %s, its nodeRef is %q", m.Name, m.Status.Phase, m.Status.NodeRef.Name)
				}
				machine = m
				return nil
			})
			lcp := &v1alpha1.LodiaControlPlane{}
			if err := k8s.Get(ctx, key, lcp); err != nil {
				t.Fatal(err)
			}
			if len(lcp.Status.Versions) == 0 || lcp.Status.Versions[0].Version != lcp.Spec.Version {
				t.Errorf("LodiaControlPlane.status.versions is %+v, want %s", lcp.Status.Versions, lcp.Spec.Version)
			}
			return ctx
		}).
		Assess("the Node carries the DevMachine's providerID, has no uninitialized taint and is Ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			dm := &unstructured.Unstructured{}
			dm.SetGroupVersionKind(devMachine)
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: machine.Spec.InfrastructureRef.Name}, dm); err != nil {
				t.Fatal(err)
			}
			providerID, _, err := unstructured.NestedString(dm.Object, "spec", "providerID")
			if err != nil || providerID == "" {
				t.Fatalf("DevMachine %s has no spec.providerID: %v", dm.GetName(), err)
			}
			until(ctx, t, 3*time.Minute, "Node "+machine.Status.NodeRef.Name+" has the providerID, no uninitialized taint, and is Ready", func(ctx context.Context) error {
				joined := &corev1.Node{}
				if err := tenant.client.Get(ctx, client.ObjectKey{Name: machine.Status.NodeRef.Name}, joined); err != nil {
					return err
				}
				if joined.Spec.ProviderID != providerID {
					return fmt.Errorf("the Node's providerID is %q, the DevMachine's %q", joined.Spec.ProviderID, providerID)
				}
				for _, taint := range joined.Spec.Taints {
					if taint.Key == clusterv1.NodeUninitializedTaint.Key {
						return fmt.Errorf("the Node still has the taint %s", taint.Key)
					}
				}
				for _, c := range joined.Status.Conditions {
					if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
						node = joined
						return nil
					}
				}
				return fmt.Errorf("the Node is not Ready: %+v", joined.Status.Conditions)
			})
			return ctx
		}).
		Assess("the konnectivity agent is ready on the Node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, 3*time.Minute, "the konnectivity agent is ready", func(ctx context.Context) error {
				agents := &appsv1.DaemonSet{}
				if err := tenant.client.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: "konnectivity-agent"}, agents); err != nil {
					return err
				}
				if s := agents.Status; s.DesiredNumberScheduled != 1 || s.NumberReady != 1 {
					return fmt.Errorf("%d of %d agents ready", s.NumberReady, s.DesiredNumberScheduled)
				}
				return nil
			})
			return ctx
		}).
		Assess("a pod runs on the Node, and its logs reach the client through the API server and the tunnel", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			web := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "web"},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "web",
					Image: agnhost,
					Args:  []string{"netexec", "--http-port=8080"},
				}}},
			}
			if err := tenant.client.Create(ctx, web); err != nil {
				t.Fatal(err)
			}
			pods := kubernetes.NewForConfigOrDie(tenant.cfg).CoreV1().Pods(web.Namespace)
			until(ctx, t, 3*time.Minute, "pod web runs on Node "+node.Name+" and its logs are readable", func(ctx context.Context) error {
				if err := tenant.client.Get(ctx, client.ObjectKeyFromObject(web), web); err != nil {
					return err
				}
				if web.Spec.NodeName != node.Name {
					return fmt.Errorf("pod web is scheduled to %q", web.Spec.NodeName)
				}
				if err := running(web); err != nil {
					return err
				}
				logs, err := pods.GetLogs(web.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
				if err != nil {
					return err
				}
				if !strings.Contains(string(logs), "Started HTTP server on port 8080") {
					return fmt.Errorf("the logs of pod web lack agnhost's start line:\n%s", logs)
				}
				return nil
			})
			return ctx
		}).
		Assess("the Cluster is Available", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, time.Minute, "Available is True", func(ctx context.Context) error {
				cluster := &clusterv1.Cluster{}
				if err := k8s.Get(ctx, key, cluster); err != nil {
					return err
				}
				if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterAvailableCondition); c == nil || c.Status != metav1.ConditionTrue {
					return fmt.Errorf("Available is %+v", c)
				}
				return nil
			})
			return ctx
		}).
		Assess("deleting the Cluster removes the LodiaControlPlane, the ControlPlane, the etcd claim and the worker's container", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			etcd := lodiav1alpha1.EtcdName(key.Name)
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: etcd + "-" + etcd + "-0"}}
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatalf("the etcd claim, whose removal this step checks: %v", err)
			}
			containers, err := clusterContainers(ctx, key.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(containers) != 1 {
				t.Fatalf("%d containers are labelled for the Cluster, want the worker's alone: %v", len(containers), containers)
			}
			if err := k8s.Delete(ctx, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, 10*time.Minute, "everything the Cluster made is gone", func(ctx context.Context) error {
				for _, obj := range []client.Object{
					&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
					&v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
					&lodiav1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
					claim,
				} {
					if err := gone(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)); err != nil {
						return fmt.Errorf("%T %s: %w", obj, obj.GetName(), err)
					}
				}
				left, err := clusterContainers(ctx, key.Name)
				if err != nil {
					return err
				}
				if len(left) > 0 {
					return fmt.Errorf("containers %s are left", strings.Join(left, ", "))
				}
				return nil
			})
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if t.Failed() {
				dump(ctx, cfg)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

func newClient(t *testing.T, cfg *envconf.Config) client.Client {
	t.Helper()
	k8s, err := client.New(cfg.Client().RESTConfig(), client.Options{Scheme: controller.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return k8s
}

func render(file, cluster string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		return nil, err
	}
	return yamlprocessor.NewSimpleProcessor().Process(raw, func(name string) (string, error) {
		if name != "CLUSTER_NAME" {
			return "", fmt.Errorf("%s is not set", name)
		}
		return cluster, nil
	})
}

type tenantAccess struct {
	cfg       *rest.Config
	client    client.Client
	resources *resources.Resources
}

func openTenant(ctx context.Context, t *testing.T, k8s client.Client, key client.ObjectKey) *tenantAccess {
	t.Helper()
	kubeconfig := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)}, kubeconfig); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig.Data[secret.KubeconfigDataName])
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := resources.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the tenant's API server %s is reached through %s", cfg.Host, kubeconfig.Name)
	return &tenantAccess{cfg: cfg, client: c, resources: r}
}

func until(ctx context.Context, t *testing.T, timeout time.Duration, what string, check func(context.Context) error) {
	t.Helper()
	start := time.Now()
	var last error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		err := check(ctx)
		if ctx.Err() == nil {
			last = err
		}
		return err == nil, nil
	})
	if err != nil {
		t.Fatalf("%s: %v: %v", what, err, last)
	}
	t.Logf("%s, after %s", what, time.Since(start).Round(time.Second))
}

func running(pod *corev1.Pod) error {
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("pod %s is %s", pod.Name, pod.Status.Phase)
	}
	for _, container := range pod.Status.ContainerStatuses {
		if !container.Ready {
			return fmt.Errorf("container %s of pod %s is not ready", container.Name, pod.Name)
		}
	}
	return nil
}

func clusterContainers(ctx context.Context, cluster string) ([]string, error) {
	out, err := docker(ctx, "ps", "--all", "--quiet", "--filter", "label="+clusterLabel+"="+cluster)
	return strings.Fields(out), err
}

func gone(err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("still there")
	case apierrors.IsNotFound(err):
		return nil
	default:
		return err
	}
}
