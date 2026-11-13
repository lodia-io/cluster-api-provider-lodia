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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/test/clusterctlproxy"
)

func TestAWorkerJoinsAClusterOfLodiasControlPlane(t *testing.T) {
	var (
		k8s     client.Client
		key     client.ObjectKey
		tenant  *tenantAccess
		machine *clusterv1.Machine
		node    *corev1.Node
		second  *clusterv1.Machine
		deleted *clusterv1.Machine
		kept    *clusterv1.Machine
		settled snapshot
	)
	f := features.New("a worker joins a cluster of Lodia's control plane").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			name, err := clusterName(cfg)
			if err != nil {
				t.Fatal(err)
			}
			key = client.ObjectKey{Namespace: namespace, Name: name}
			if k8s, err = newClient(cfg); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the objects are applied, and the DevCluster, whose infrastructure the suite manages, is marked provisioned", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			bounded, cancel := context.WithTimeout(ctx, bounds.Apply)
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
			until(ctx, t, bounds.Initialized, "ControlPlaneInitialized is True", func(ctx context.Context) error {
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
			until(ctx, t, bounds.Endpoint, "Cluster.spec.controlPlaneEndpoint is the ControlPlane's status.endpoint.apiServer", func(ctx context.Context) error {
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
		Assess("the provider copied Lodia's kubeconfig and the CA's certificate, never its key, and owns both", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			lcp := &v1alpha1.LodiaControlPlane{}
			if err := k8s.Get(ctx, key, lcp); err != nil {
				t.Fatal(err)
			}
			if err := requireCopies(ctx, k8s, key, lcp); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("kindnet, from the management node's own manifest, is applied in the tenant", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			tenant = openTenant(ctx, t, k8s, key, tenantRequestTimeout)
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
			until(ctx, t, bounds.Kindnet, "kindnet is applied", func(ctx context.Context) error {
				return decoder.DecodeEach(ctx, bytes.NewReader(rendered.Bytes()), ignoreExisting(decoder.CreateHandler(tenant.resources)))
			})
			return ctx
		}).
		Assess("the Machine is Running with a nodeRef, and Lodia reported the version the MachineSet's preflight reads", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Running, "the Machine is Running with a nodeRef", func(ctx context.Context) error {
				workers, err := runningWorkers(ctx, k8s, key, 1)
				if err != nil {
					return err
				}
				machine = &workers[0]
				return nil
			})
			lcp := &v1alpha1.LodiaControlPlane{}
			if err := k8s.Get(ctx, key, lcp); err != nil {
				t.Fatal(err)
			}
			if len(lcp.Status.Versions) == 0 || lcp.Status.Versions[0].Version != lcp.Spec.Version {
				t.Fatalf("LodiaControlPlane.status.versions is %+v, want %s", lcp.Status.Versions, lcp.Spec.Version)
			}
			return ctx
		}).
		Assess("the Node carries the DevMachine's providerID, has no uninitialized taint and is Ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Ready, "Node "+machine.Status.NodeRef.Name+" has the providerID, no uninitialized taint, and is Ready", func(ctx context.Context) error {
				joined, err := requireJoined(ctx, k8s, tenant, machine)
				if err != nil {
					return err
				}
				node = joined
				return nil
			})
			return ctx
		}).
		Assess("the worker holds the file that the join rendered from a template of the control plane's version", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			lcp := &v1alpha1.LodiaControlPlane{}
			if err := k8s.Get(ctx, key, lcp); err != nil {
				t.Fatal(err)
			}
			containers, err := clusterContainers(ctx, key.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(containers) != 1 {
				t.Fatalf("%d containers are labelled for the Cluster, want the worker's alone: %v", len(containers), containers)
			}
			out, err := docker(ctx, "exec", containers[0], "cat", templatedFile)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(out); got != lcp.Spec.Version {
				t.Fatalf("%s holds %q, want the control plane's version %q", templatedFile, got, lcp.Spec.Version)
			}
			return ctx
		}).
		Assess("the konnectivity agent is ready on the Node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Konnectivity, "the konnectivity agent is ready", func(ctx context.Context) error {
				return requireAgents(ctx, tenant, 1)
			})
			return ctx
		}).
		Assess("a pod runs on the Node, and its logs are readable through the tenant's API server", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			web := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "web"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{agnhostContainer("web")}},
			}
			if err := tenant.client.Create(ctx, web); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, bounds.Pod, "pod web runs on Node "+node.Name+" and its logs are readable", func(ctx context.Context) error {
				if err := tenant.client.Get(ctx, client.ObjectKeyFromObject(web), web); err != nil {
					return err
				}
				if web.Spec.NodeName != node.Name {
					return fmt.Errorf("pod web is scheduled to %q", web.Spec.NodeName)
				}
				if err := running(web); err != nil {
					return err
				}
				return requireLogs(ctx, tenant, web)
			})
			return ctx
		}).
		Assess("the Cluster is Available", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Available, "Available is True", func(ctx context.Context) error {
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
		Assess("clusterctl's move waits for the block-move annotation of the LodiaControlPlane, and is ended there", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := newClusterctl(ctx, cfg, filepath.Join(repositoryDir(cfg), clusterctlConfigFile))
			if err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, bounds.Move)
			defer cancel()
			options := clusterctl.MoveOptions{FromKubeconfig: clusterctlproxy.Kubeconfig, ToKubeconfig: clusterctlproxy.Kubeconfig, Namespace: key.Namespace}
			if err := clusterctlproxy.MoveUntilBlocked(bounded, c, options, "LodiaControlPlane", key.Name); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("once both controllers report the pause and the Cluster is unpaused, Paused is False on both and the Cluster's Available is recomputed", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := requireUnpaused(ctx, k8s, key, bounds.Unpaused, 2*time.Second); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("a Deployment of one replica runs on the only worker", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := tenant.client.Create(ctx, workload()); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, bounds.Deployed, "the pod of Deployment "+workloadName+" runs on Node "+node.Name, func(ctx context.Context) error {
				_, err := requirePodOn(ctx, tenant, node.Name)
				return err
			})
			return ctx
		}).
		Assess("scaled to 2, the second Machine is Running with a nodeRef", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := scaleWorkers(ctx, k8s, key, 2); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, bounds.ScaleUp, "the second Machine is Running with a nodeRef", func(ctx context.Context) error {
				workers, err := runningWorkers(ctx, k8s, key, 2)
				if err != nil {
					return err
				}
				other, err := otherThan(machine, workers)
				if err != nil {
					return err
				}
				second = other
				return nil
			})
			return ctx
		}).
		Assess("the second Node carries the DevMachine's providerID, has no uninitialized taint and is Ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.ScaleReady, "Node "+second.Status.NodeRef.Name+" has the providerID, no uninitialized taint, and is Ready", func(ctx context.Context) error {
				_, err := requireJoined(ctx, k8s, tenant, second)
				return err
			})
			return ctx
		}).
		Assess("the konnectivity agents are ready on both Nodes", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.ScaleAgents, "the konnectivity agents are ready on both Nodes", func(ctx context.Context) error {
				return requireAgents(ctx, tenant, 2)
			})
			return ctx
		}).
		Assess("scaled back to 1, the Machine that hosts the Deployment's pod is drained and gone with its DevMachine and its Node, and the other stays", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			pod, err := workloadPod(ctx, tenant)
			if err != nil {
				t.Fatal(err)
			}
			workers, err := runningWorkers(ctx, k8s, key, 2)
			if err != nil {
				t.Fatal(err)
			}
			if deleted, err = machineOn(workers, pod.Spec.NodeName); err != nil {
				t.Fatal(err)
			}
			if kept, err = otherThan(deleted, workers); err != nil {
				t.Fatal(err)
			}
			if err := markForDeletion(ctx, k8s, deleted); err != nil {
				t.Fatal(err)
			}
			if err := scaleWorkers(ctx, k8s, key, 1); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, bounds.ScaleDown, "Machine "+deleted.Name+" is gone, drained, with its DevMachine and its Node, and Machine "+kept.Name+" stays", func(ctx context.Context) error {
				return requireScaledDown(ctx, k8s, tenant, key, deleted, kept)
			})
			return ctx
		}).
		Assess("the Deployment's pod runs on the Node that stays, and its logs are readable through the tenant's API server", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Rescheduled, "the pod of Deployment "+workloadName+" runs on Node "+kept.Status.NodeRef.Name+" and its logs are readable", func(ctx context.Context) error {
				pod, err := requirePodOn(ctx, tenant, kept.Status.NodeRef.Name)
				if err != nil {
					return err
				}
				return requireLogs(ctx, tenant, pod)
			})
			return ctx
		}).
		Assess("every object of the namespace has the owners that its kind wants, Lodia's ControlPlane and the two copied Secrets among them", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Owners, "every object has the owners that its kind wants", func(ctx context.Context) error {
				return requireOwners(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("with the Cluster paused, the owner references of the LodiaControlPlane and of Cluster API's objects are removed, and once it is unpaused they are back", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := whilePaused(ctx, k8s, key, bounds.OwnersPaused, 2*time.Second, func(ctx context.Context) error {
				return removeOwnerReferences(ctx, k8s, key.Namespace)
			}); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, bounds.OwnersRestored, "every object has the owners that its kind wants again", func(ctx context.Context) error {
				return requireOwners(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("every object of the namespace holds exactly the finalizers that its kind wants, those of Lodia and of etcd-druid among them", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, bounds.Finalizers, "every object holds the finalizers that its kind wants", func(ctx context.Context) error {
				return requireFinalizers(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("with the Cluster paused, the finalizers of the LodiaControlPlane and of Cluster API's objects are removed, and once it is unpaused they are back", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := whilePaused(ctx, k8s, key, bounds.FinalizersPaused, 2*time.Second, func(ctx context.Context) error {
				return removeFinalizers(ctx, k8s, key.Namespace)
			}); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, bounds.FinalizersRestored, "every object holds the finalizers that its kind wants again", func(ctx context.Context) error {
				return requireFinalizers(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("the resourceVersions of the namespace's objects stop changing", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			var err error
			if settled, err = settle(ctx, k8s, key.Namespace, bounds.Settled, settleInterval); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("and stay as they are for the whole window, but for what Lodia and etcd-druid write", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := hold(ctx, k8s, key.Namespace, bounds.Stable, holdInterval, settled); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("deleting the Cluster removes the LodiaControlPlane, the ControlPlane, the etcd claim, the copied Secrets and the worker's container", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			etcd := lodiav1alpha1.EtcdName(key.Name)
			owned := []client.Object{
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: etcd + "-" + etcd + "-0"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.Kubeconfig)}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: secret.Name(key.Name, secret.ClusterCA)}},
			}
			for _, obj := range owned {
				if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatalf("%T %s, whose removal this step checks: %v", obj, obj.GetName(), err)
				}
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
			until(ctx, t, bounds.Deleted, "everything the Cluster made is gone", func(ctx context.Context) error {
				for _, obj := range append([]client.Object{
					&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
					&v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
					&lodiav1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}},
				}, owned...) {
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
