// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
)

func dump(ctx context.Context, cfg *envconf.Config) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dir := filepath.Join(workDir(cfg), "artifacts")
	c, err := client.New(cfg.Client().RESTConfig(), client.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
		return
	}
	var errs []error
	for _, kind := range objectKinds {
		errs = append(errs, dumpObjects(ctx, c, dir, kind, namespace))
	}
	errs = append(errs, dumpObjects(ctx, c, dir, datastoreClass, ""))
	for _, ns := range managers {
		errs = append(errs, dumpObjects(ctx, c, dir, corev1.SchemeGroupVersion.WithKind("Pod"), ns), dumpLogs(ctx, cfg, dir, ns))
	}
	errs = append(errs, dumpWorkers(ctx, cfg, dir))
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
	}
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
