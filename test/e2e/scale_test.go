// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

const agnhost = "registry.k8s.io/e2e-test-images/agnhost:2.66.1@sha256:6e2eef87d0dc77e070a7549bc5fdfa290c0fefe3359eb33c2ad1eda692fe66a4"

const (
	workloadName   = "workload"
	agnhostStarted = "Started HTTP server on port 8080"
)

// The reasons of the events that the Machine controller records on a Machine
// that it deletes:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/machine/machine_controller.go#L561-L576
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/machine/machine_controller.go#L680
const (
	drainedReason     = "SuccessfulDrainNode"
	drainFailedReason = "FailedDrainNode"
	nodeFailedReason  = "FailedDeleteNode"
)

func agnhostContainer(name string) corev1.Container {
	return corev1.Container{Name: name, Image: agnhost, Args: []string{"netexec", "--http-port=8080"}}
}

func workloadLabels() map[string]string { return map[string]string{"app": workloadName} }

func workload() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: workloadName},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: workloadLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: workloadLabels()},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{agnhostContainer(workloadName)}},
			},
		},
	}
}

func workersName(cluster string) string { return cluster + "-md-0" }

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

func scaleWorkers(ctx context.Context, k8s client.Client, key client.ObjectKey, replicas int32) error {
	workers := &clusterv1.MachineDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: workersName(key.Name)}}
	return k8s.Patch(ctx, workers, client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)))
}

func markForDeletion(ctx context.Context, k8s client.Client, machine *clusterv1.Machine) error {
	marked := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: machine.Namespace, Name: machine.Name}}
	return k8s.Patch(ctx, marked, client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"metadata":{"annotations":{%q:"true"}}}`, clusterv1.DeleteMachineAnnotation)))
}

func runningWorkers(ctx context.Context, k8s client.Reader, key client.ObjectKey, want int) ([]clusterv1.Machine, error) {
	machines := &clusterv1.MachineList{}
	if err := k8s.List(ctx, machines, client.InNamespace(key.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: key.Name}); err != nil {
		return nil, err
	}
	if len(machines.Items) != want {
		return nil, fmt.Errorf("%d Machines, want %d", len(machines.Items), want)
	}
	for _, m := range machines.Items {
		if m.Status.Phase != string(clusterv1.MachinePhaseRunning) || m.Status.NodeRef.Name == "" {
			return nil, fmt.Errorf("Machine %s is %s, its nodeRef is %q", m.Name, m.Status.Phase, m.Status.NodeRef.Name)
		}
	}
	return machines.Items, nil
}

func machineOn(machines []clusterv1.Machine, node string) (*clusterv1.Machine, error) {
	for i := range machines {
		if machines[i].Status.NodeRef.Name == node {
			return &machines[i], nil
		}
	}
	return nil, fmt.Errorf("no Machine has the nodeRef %q", node)
}

func otherThan(machine *clusterv1.Machine, machines []clusterv1.Machine) (*clusterv1.Machine, error) {
	for i := range machines {
		if machines[i].UID != machine.UID {
			return &machines[i], nil
		}
	}
	return nil, fmt.Errorf("every Machine is %s", machine.Name)
}

func requireJoined(ctx context.Context, k8s client.Reader, tenant *tenantAccess, machine *clusterv1.Machine) (*corev1.Node, error) {
	dm := &unstructured.Unstructured{}
	dm.SetGroupVersionKind(devMachine)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.InfrastructureRef.Name}, dm); err != nil {
		return nil, err
	}
	providerID, _, err := unstructured.NestedString(dm.Object, "spec", "providerID")
	if err != nil {
		return nil, err
	}
	if providerID == "" {
		return nil, fmt.Errorf("DevMachine %s has no spec.providerID", dm.GetName())
	}
	joined := &corev1.Node{}
	if err := tenant.client.Get(ctx, client.ObjectKey{Name: machine.Status.NodeRef.Name}, joined); err != nil {
		return nil, err
	}
	if joined.Spec.ProviderID != providerID {
		return nil, fmt.Errorf("the Node's providerID is %q, the DevMachine's %q", joined.Spec.ProviderID, providerID)
	}
	for _, taint := range joined.Spec.Taints {
		if taint.Key == clusterv1.NodeUninitializedTaint.Key {
			return nil, fmt.Errorf("the Node still has the taint %s", taint.Key)
		}
	}
	for _, c := range joined.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return joined, nil
		}
	}
	return nil, fmt.Errorf("the Node is not Ready: %+v", joined.Status.Conditions)
}

func requireAgents(ctx context.Context, tenant *tenantAccess, want int32) error {
	agents := &appsv1.DaemonSet{}
	if err := tenant.client.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: "konnectivity-agent"}, agents); err != nil {
		return err
	}
	if s := agents.Status; s.DesiredNumberScheduled != want || s.NumberReady != want {
		return fmt.Errorf("%d of %d agents ready, want %d of %d", s.NumberReady, s.DesiredNumberScheduled, want, want)
	}
	return nil
}

func workloadPod(ctx context.Context, tenant *tenantAccess) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := tenant.client.List(ctx, pods, client.InNamespace(metav1.NamespaceDefault), client.MatchingLabels(workloadLabels())); err != nil {
		return nil, err
	}
	if len(pods.Items) != 1 {
		return nil, fmt.Errorf("%d pods of the Deployment %s, want 1", len(pods.Items), workloadName)
	}
	if pod := &pods.Items[0]; pod.DeletionTimestamp.IsZero() {
		return pod, nil
	}
	return nil, fmt.Errorf("the pod %s of the Deployment %s is being deleted", pods.Items[0].Name, workloadName)
}

func requirePodOn(ctx context.Context, tenant *tenantAccess, node string) (*corev1.Pod, error) {
	pod, err := workloadPod(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if pod.Spec.NodeName != node {
		return nil, fmt.Errorf("pod %s is scheduled to %q, want %q", pod.Name, pod.Spec.NodeName, node)
	}
	if err := running(pod); err != nil {
		return nil, err
	}
	return pod, nil
}

func requireLogs(ctx context.Context, tenant *tenantAccess, pod *corev1.Pod) error {
	pods, err := kubernetes.NewForConfig(tenant.cfg)
	if err != nil {
		return err
	}
	logs, err := pods.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(string(logs), agnhostStarted) {
		return fmt.Errorf("the logs of pod %s lack agnhost's start line:\n%s", pod.Name, logs)
	}
	return nil
}

func requireDrained(ctx context.Context, k8s client.Reader, machine *clusterv1.Machine) error {
	events := &corev1.EventList{}
	if err := k8s.List(ctx, events, client.InNamespace(machine.Namespace)); err != nil {
		return err
	}
	reasons := make(map[string]bool)
	for _, e := range events.Items {
		if e.InvolvedObject.Kind == "Machine" && e.InvolvedObject.UID == machine.UID {
			reasons[e.Reason] = true
		}
	}
	for _, reason := range []string{drainFailedReason, nodeFailedReason} {
		if reasons[reason] {
			return fmt.Errorf("Machine %s has an event %s", machine.Name, reason)
		}
	}
	if !reasons[drainedReason] {
		return fmt.Errorf("Machine %s has no event %s", machine.Name, drainedReason)
	}
	return nil
}

func requireWorkersAt(ctx context.Context, k8s client.Reader, key client.ObjectKey, want int32) error {
	workers := &clusterv1.MachineDeployment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: workersName(key.Name)}, workers); err != nil {
		return err
	}
	for _, replicas := range []struct {
		field string
		got   *int32
	}{
		{"spec.replicas", workers.Spec.Replicas},
		{"status.replicas", workers.Status.Replicas},
		{"status.readyReplicas", workers.Status.ReadyReplicas},
		{"status.availableReplicas", workers.Status.AvailableReplicas},
	} {
		if replicas.got == nil || *replicas.got != want {
			return fmt.Errorf("MachineDeployment %s has %s %s, want %d", workers.Name, replicas.field, count(replicas.got), want)
		}
	}
	return nil
}

func count(replicas *int32) string {
	if replicas == nil {
		return "unset"
	}
	return strconv.Itoa(int(*replicas))
}

func requireAvailable(ctx context.Context, k8s client.Reader, key client.ObjectKey) error {
	cluster := &clusterv1.Cluster{}
	if err := k8s.Get(ctx, key, cluster); err != nil {
		return err
	}
	if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterAvailableCondition); c == nil || c.Status != metav1.ConditionTrue {
		return fmt.Errorf("Available is %+v", c)
	}
	return nil
}

func requireScaledDown(ctx context.Context, k8s client.Reader, tenant *tenantAccess, key client.ObjectKey, deleted, kept *clusterv1.Machine) error {
	if err := gone(k8s.Get(ctx, client.ObjectKeyFromObject(deleted), &clusterv1.Machine{})); err != nil {
		return fmt.Errorf("Machine %s: %w", deleted.Name, err)
	}
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(devMachine)
	if err := gone(k8s.Get(ctx, client.ObjectKey{Namespace: deleted.Namespace, Name: deleted.Spec.InfrastructureRef.Name}, infra)); err != nil {
		return fmt.Errorf("DevMachine %s: %w", deleted.Spec.InfrastructureRef.Name, err)
	}
	current := &clusterv1.Machine{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(kept), current); err != nil {
		return fmt.Errorf("the Machine %s that stays: %w", kept.Name, err)
	}
	if current.UID != kept.UID {
		return fmt.Errorf("the Machine %s that stays has the UID %s, it had %s", kept.Name, current.UID, kept.UID)
	}
	if current.Status.Phase != string(clusterv1.MachinePhaseRunning) || current.Status.NodeRef.Name != kept.Status.NodeRef.Name {
		return fmt.Errorf("the Machine %s that stays is %s, its nodeRef is %q, it had %q", kept.Name, current.Status.Phase, current.Status.NodeRef.Name, kept.Status.NodeRef.Name)
	}
	if err := requireDrained(ctx, k8s, deleted); err != nil {
		return err
	}
	if err := gone(tenant.client.Get(ctx, client.ObjectKey{Name: deleted.Status.NodeRef.Name}, &corev1.Node{})); err != nil {
		return fmt.Errorf("Node %s: %w", deleted.Status.NodeRef.Name, err)
	}
	if err := requireWorkersAt(ctx, k8s, key, 1); err != nil {
		return err
	}
	return requireAvailable(ctx, k8s, key)
}

var scaleKey = client.ObjectKey{Namespace: "capi-join", Name: "w"}

func workerMachine(name, uid, node string) *clusterv1.Machine {
	m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Namespace: scaleKey.Namespace,
		Name:      name,
		UID:       types.UID(uid),
		Labels:    map[string]string{clusterv1.ClusterNameLabel: scaleKey.Name},
	}}
	m.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{Kind: devMachine.Kind, Name: name}
	m.Status.Phase = string(clusterv1.MachinePhaseRunning)
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: node}
	return m
}

func workerDevMachine(name, providerID string) *unstructured.Unstructured {
	dm := &unstructured.Unstructured{}
	dm.SetGroupVersionKind(devMachine)
	dm.SetNamespace(scaleKey.Namespace)
	dm.SetName(name)
	if providerID != "" {
		_ = unstructured.SetNestedField(dm.Object, providerID, "spec", "providerID")
	}
	return dm
}

func workerNode(name, providerID string, ready corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID, Taints: taints},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func workerEvent(reason string, machine *clusterv1.Machine) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: machine.Namespace, Name: reason + "-" + machine.Name + "-" + string(machine.UID)},
		InvolvedObject: corev1.ObjectReference{Kind: "Machine", Namespace: machine.Namespace, Name: machine.Name, UID: machine.UID},
		Reason:         reason,
	}
}

func workloadPodOn(node string, change ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "workload-" + node, Labels: workloadLabels()},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: workloadName, Ready: true}}},
	}
	for _, c := range change {
		c(pod)
	}
	return pod
}

func tenantOf(t *testing.T, objects ...client.Object) *tenantAccess {
	t.Helper()
	return &tenantAccess{client: fake.NewClientBuilder().WithObjects(objects...).Build()}
}

func managementOf(objects ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(controller.Scheme()).WithObjects(objects...).Build()
}

func aboutAKind(e *corev1.Event, kind string) *corev1.Event {
	e.InvolvedObject.Kind = kind
	return e
}

func inNamespace(e *corev1.Event, namespace string) *corev1.Event {
	e.Namespace = namespace
	return e
}

func mutated(machine *clusterv1.Machine, change func(*clusterv1.Machine)) *clusterv1.Machine {
	change(machine)
	return machine
}

func requireFailure(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("want accepted, got %v", err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Fatalf("want an error with %q, got %v", want, err)
	}
}

func TestScalingPatchesTheMachineDeploymentOfTheWorkersAlone(t *testing.T) {
	workers := &clusterv1.MachineDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: workersName(scaleKey.Name)}, Spec: clusterv1.MachineDeploymentSpec{Replicas: new(int32(1))}}
	other := &clusterv1.MachineDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: "other"}, Spec: clusterv1.MachineDeploymentSpec{Replicas: new(int32(1))}}
	k8s := managementOf(workers, other)
	for _, replicas := range []int32{2, 1} {
		if err := scaleWorkers(t.Context(), k8s, scaleKey, replicas); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]int32{workers.Name: replicas, other.Name: 1} {
			got := &clusterv1.MachineDeployment{}
			if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: scaleKey.Namespace, Name: name}, got); err != nil {
				t.Fatal(err)
			}
			if *got.Spec.Replicas != want {
				t.Errorf("after scaling to %d, MachineDeployment %s has %d replicas, want %d", replicas, name, *got.Spec.Replicas, want)
			}
		}
	}
	if err := scaleWorkers(t.Context(), managementOf(other), scaleKey, 2); !apierrors.IsNotFound(err) {
		t.Fatalf("want a NotFound for a cluster without workers, got %v", err)
	}
}

func TestTheTestdataHoldsTheMachineDeploymentThatTheStepsScaleFromOne(t *testing.T) {
	raw, err := os.ReadFile("testdata/workers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for document := range strings.SplitSeq(string(raw), "\n---\n") {
		workers := &clusterv1.MachineDeployment{}
		if err := yaml.Unmarshal([]byte(document), workers); err != nil {
			t.Fatal(err)
		}
		if workers.Kind != "MachineDeployment" {
			continue
		}
		found++
		if want := workersName("${CLUSTER_NAME}"); workers.Name != want {
			t.Errorf("the MachineDeployment is named %s, the steps scale %s", workers.Name, want)
		}
		if workers.Spec.Replicas == nil || *workers.Spec.Replicas != 1 {
			t.Errorf("the MachineDeployment starts at %s replicas, the steps scale from 1", count(workers.Spec.Replicas))
		}
		if workers.Spec.Deletion.Order != clusterv1.OldestMachineSetDeletionOrder {
			t.Errorf("the MachineDeployment's deletion order is %q, want %q, so that the first worker goes even if the annotation has not reached the MachineSet's cache", workers.Spec.Deletion.Order, clusterv1.OldestMachineSetDeletionOrder)
		}
	}
	if found != 1 {
		t.Fatalf("%d MachineDeployments in testdata/workers.yaml, want 1", found)
	}
}

func TestMarkingAMachineForDeletionAnnotatesThatMachineAlone(t *testing.T) {
	marked, other := workerMachine("a", "ua", "na"), workerMachine("b", "ub", "nb")
	marked.Annotations = map[string]string{"kept": "yes"}
	k8s := managementOf(marked, other)
	if err := markForDeletion(t.Context(), k8s, marked); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]map[string]string{
		"a": {"kept": "yes", clusterv1.DeleteMachineAnnotation: "true"},
		"b": nil,
	} {
		got := &clusterv1.Machine{}
		if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: scaleKey.Namespace, Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if len(got.Annotations) != len(want) {
			t.Errorf("Machine %s has the annotations %v, want %v", name, got.Annotations, want)
		}
		for k, v := range want {
			if got.Annotations[k] != v {
				t.Errorf("Machine %s has the annotations %v, want %v", name, got.Annotations, want)
			}
		}
	}
}

func TestTheWorkersAreMachinesOfTheClusterThatAreRunningWithANodeRef(t *testing.T) {
	cases := []struct {
		name    string
		want    int
		objects []client.Object
		failure string
	}{
		{"one of one", 1, []client.Object{workerMachine("a", "ua", "na")}, ""},
		{"two of two", 2, []client.Object{workerMachine("a", "ua", "na"), workerMachine("b", "ub", "nb")}, ""},
		{"one where two are wanted", 2, []client.Object{workerMachine("a", "ua", "na")}, "1 Machines, want 2"},
		{"two where one is wanted", 1, []client.Object{workerMachine("a", "ua", "na"), workerMachine("b", "ub", "nb")}, "2 Machines, want 1"},
		{"a Machine that is Provisioned", 1, []client.Object{mutated(workerMachine("a", "ua", "na"), func(m *clusterv1.Machine) {
			m.Status.Phase = string(clusterv1.MachinePhaseProvisioned)
		})}, "is Provisioned"},
		{"a Machine without a nodeRef", 1, []client.Object{workerMachine("a", "ua", "")}, "nodeRef"},
		{"a Machine of another Cluster is not counted", 1, []client.Object{workerMachine("a", "ua", "na"), mutated(workerMachine("b", "ub", "nb"), func(m *clusterv1.Machine) {
			m.Labels[clusterv1.ClusterNameLabel] = "other"
		})}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runningWorkers(t.Context(), managementOf(c.objects...), scaleKey, c.want)
			requireFailure(t, err, c.failure)
		})
	}
}

func TestTheMachineThatHostsAPodIsTheOneWhoseNodeRefNamesItsNode(t *testing.T) {
	machines := []clusterv1.Machine{*workerMachine("a", "ua", "na"), *workerMachine("b", "ub", "nb")}
	for node, want := range map[string]string{"na": "a", "nb": "b"} {
		host, err := machineOn(machines, node)
		if err != nil || host.Name != want {
			t.Errorf("the Machine on %s is %v, %v, want %s", node, host, err, want)
		}
	}
	if host, err := machineOn(machines, "nc"); err == nil {
		t.Errorf("the Machine on a Node that none has is %s", host.Name)
	}
	if host, err := machineOn(machines, ""); err == nil {
		t.Errorf("the Machine on no Node is %s", host.Name)
	}
	for i, want := range []string{"b", "a"} {
		if other, err := otherThan(&machines[i], machines); err != nil || other.Name != want {
			t.Errorf("the Machine other than %s is %v, %v, want %s", machines[i].Name, other, err, want)
		}
	}
	if other, err := otherThan(&machines[0], machines[:1]); err == nil {
		t.Errorf("the Machine other than the only one is %s", other.Name)
	}
	if other, err := otherThan(workerMachine("c", "ua", "nc"), machines[:1]); err == nil {
		t.Errorf("a Machine of the same UID under another name is the same Machine, not %s", other.Name)
	}
}

func TestANodeHasJoinedWhenItsProviderIDIsTheDevMachinesAndItIsUntaintedAndReady(t *testing.T) {
	machine := workerMachine("a", "ua", "na")
	uninitialized := corev1.Taint{Key: clusterv1.NodeUninitializedTaint.Key, Effect: corev1.TaintEffectNoSchedule}
	cases := []struct {
		name       string
		management []client.Object
		tenant     []client.Object
		failure    string
	}{
		{"a Node that has joined", []client.Object{workerDevMachine("a", "docker:///a")}, []client.Object{workerNode("na", "docker:///a", corev1.ConditionTrue)}, ""},
		{"a taint of another key", []client.Object{workerDevMachine("a", "docker:///a")}, []client.Object{workerNode("na", "docker:///a", corev1.ConditionTrue, corev1.Taint{Key: "other", Effect: corev1.TaintEffectNoSchedule})}, ""},
		{"another providerID", []client.Object{workerDevMachine("a", "docker:///a")}, []client.Object{workerNode("na", "docker:///b", corev1.ConditionTrue)}, "providerID"},
		{"no providerID on the Node", []client.Object{workerDevMachine("a", "docker:///a")}, []client.Object{workerNode("na", "", corev1.ConditionTrue)}, "providerID"},
		{"no providerID on the DevMachine", []client.Object{workerDevMachine("a", "")}, []client.Object{workerNode("na", "", corev1.ConditionTrue)}, "no spec.providerID"},
		{"the uninitialized taint", []client.Object{workerDevMachine("a", "docker:///a")}, []client.Object{workerNode("na", "docker:///a", corev1.ConditionTrue, uninitialized)}, "taint"},
		{"a Node that is not Ready", []client.Object{workerDevMachine("a", "docker:///a")}, []client.Object{workerNode("na", "docker:///a", corev1.ConditionFalse)}, "not Ready"},
		{"no DevMachine", nil, []client.Object{workerNode("na", "docker:///a", corev1.ConditionTrue)}, "not found"},
		{"a Node of the management cluster only", []client.Object{workerDevMachine("a", "docker:///a"), workerNode("na", "docker:///a", corev1.ConditionTrue)}, nil, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node, err := requireJoined(t.Context(), managementOf(c.management...), tenantOf(t, c.tenant...), machine)
			requireFailure(t, err, c.failure)
			if err == nil && node.Name != "na" {
				t.Errorf("the Node is %s, want na", node.Name)
			}
		})
	}
}

func TestTheAgentsAreReadyWhenEveryNodeHasOne(t *testing.T) {
	agents := func(desired, ready int32) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "konnectivity-agent"},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: desired, NumberReady: ready},
		}
	}
	cases := []struct {
		name    string
		want    int32
		agents  *appsv1.DaemonSet
		failure string
	}{
		{"1 of 1", 1, agents(1, 1), ""},
		{"2 of 2", 2, agents(2, 2), ""},
		{"1 of 1 where 2 of 2 are wanted", 2, agents(1, 1), "1 of 1 agents ready, want 2 of 2"},
		{"1 of 2 where 2 of 2 are wanted", 2, agents(2, 1), "1 of 2 agents ready, want 2 of 2"},
		{"2 of 2 where 1 of 1 is wanted", 1, agents(2, 2), "2 of 2 agents ready, want 1 of 1"},
		{"0 of 0", 1, agents(0, 0), "0 of 0 agents ready"},
		{"3 of 3 where 2 of 2 are wanted", 2, agents(3, 3), "3 of 3 agents ready"},
		{"no agents", 1, nil, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var objects []client.Object
			if c.agents != nil {
				objects = append(objects, c.agents)
			}
			requireFailure(t, requireAgents(t.Context(), tenantOf(t, objects...), c.want), c.failure)
		})
	}
}

func TestTheWorkloadIsOnePodWithNoPlacementOfItsOwn(t *testing.T) {
	spec := workload().Spec
	pod := spec.Template.Spec
	if spec.Replicas == nil || *spec.Replicas != 1 {
		t.Errorf("the Deployment has %s replicas, want 1", count(spec.Replicas))
	}
	if pod.NodeName != "" || len(pod.NodeSelector) > 0 || pod.Affinity != nil || len(pod.TopologySpreadConstraints) > 0 {
		t.Errorf("the pod is placed by hand: %+v", pod)
	}
	for k, v := range workloadLabels() {
		if spec.Selector.MatchLabels[k] != v || spec.Template.Labels[k] != v {
			t.Errorf("the Deployment's selector %v and its pods' labels %v lack %s=%s, which the checks list by", spec.Selector.MatchLabels, spec.Template.Labels, k, v)
		}
	}
}

func TestTheWorkloadsPodRunsOnTheNodeAndIsTheOnlyOne(t *testing.T) {
	cases := []struct {
		name    string
		pods    []client.Object
		failure string
	}{
		{"a running pod on the Node", []client.Object{workloadPodOn("na")}, ""},
		{"a pod of another Deployment is not counted", []client.Object{workloadPodOn("na"), workloadPodOn("nb", func(p *corev1.Pod) { p.Labels = map[string]string{"app": "other"} })}, ""},
		{"no pod", nil, "0 pods"},
		{"a pod on another Node", []client.Object{workloadPodOn("nb")}, `scheduled to "nb", want "na"`},
		{"a pod on no Node", []client.Object{workloadPodOn("")}, `scheduled to ""`},
		{"a pod that is Pending", []client.Object{workloadPodOn("na", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })}, "is Pending"},
		{"a container that is not ready", []client.Object{workloadPodOn("na", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false })}, "not ready"},
		{"the old pod beside the new one", []client.Object{workloadPodOn("na"), workloadPodOn("nb")}, "2 pods"},
		{"a pod that is being deleted", []client.Object{workloadPodOn("na", func(p *corev1.Pod) {
			p.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			p.Finalizers = []string{"test"}
		})}, "being deleted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod, err := requirePodOn(t.Context(), tenantOf(t, c.pods...), "na")
			requireFailure(t, err, c.failure)
			if err == nil && pod.Spec.NodeName != "na" {
				t.Errorf("the pod is on %s", pod.Spec.NodeName)
			}
		})
	}
}

func TestTheLogsAreReadThroughTheTenantsAPIServerAndHoldAgnhostsStartLine(t *testing.T) {
	pod := workloadPodOn("na")
	for name, c := range map[string]struct {
		status  int
		body    string
		failure string
	}{
		"the start line":      {http.StatusOK, "I1113 " + agnhostStarted + "\n", ""},
		"other lines":         {http.StatusOK, "I1113 something else\n", "lack agnhost's start line"},
		"no lines":            {http.StatusOK, "", "lack agnhost's start line"},
		"an error of the API": {http.StatusInternalServerError, "boom", "boom"},
	} {
		t.Run(name, func(t *testing.T) {
			url, asked := recordedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.status != http.StatusOK {
					http.Error(w, c.body, c.status)
					return
				}
				respond(t, w, c.body)
			}))
			cfg, err := clientcmd.RESTConfigFromKubeConfig(serverKubeconfig(t, url, "tenant"))
			if err != nil {
				t.Fatal(err)
			}
			requireFailure(t, requireLogs(t.Context(), &tenantAccess{cfg: cfg}, pod), c.failure)
			if got := asked(); len(got) != 1 || got[0].path != "/api/v1/namespaces/default/pods/"+pod.Name+"/log" {
				t.Errorf("the tenant was asked %v, want the log of pod %s alone", got, pod.Name)
			}
		})
	}
}

func TestTheDrainOfAMachineIsHeldByTheEventsOfThatMachineAlone(t *testing.T) {
	deleted, kept := workerMachine("a", "ua", "na"), workerMachine("b", "ub", "nb")
	lookalike := workerMachine("a", "other", "na")
	cases := []struct {
		name    string
		events  []client.Object
		failure string
	}{
		{"a successful drain", []client.Object{workerEvent(drainedReason, deleted)}, ""},
		{"another event of the Machine", []client.Object{workerEvent(drainedReason, deleted), workerEvent("Other", deleted)}, ""},
		{"no events", nil, "no event SuccessfulDrainNode"},
		{"a successful drain of the Machine that stays", []client.Object{workerEvent(drainedReason, kept)}, "no event SuccessfulDrainNode"},
		{"a successful drain of a Machine of the same name and another UID", []client.Object{workerEvent(drainedReason, lookalike)}, "no event SuccessfulDrainNode"},
		{"a failed drain", []client.Object{workerEvent(drainedReason, deleted), workerEvent(drainFailedReason, deleted)}, "has an event FailedDrainNode"},
		{"a failed deletion of the Node", []client.Object{workerEvent(drainedReason, deleted), workerEvent(nodeFailedReason, deleted)}, "has an event FailedDeleteNode"},
		{"a failed drain of the Machine that stays", []client.Object{workerEvent(drainedReason, deleted), workerEvent(drainFailedReason, kept)}, ""},
		{"a failed deletion of the Node of the Machine that stays", []client.Object{workerEvent(drainedReason, deleted), workerEvent(nodeFailedReason, kept)}, ""},
		{"an event of another kind with the Machine's UID", []client.Object{aboutAKind(workerEvent(drainedReason, deleted), "Node")}, "no event SuccessfulDrainNode"},
		{"an event in another namespace", []client.Object{inNamespace(workerEvent(drainedReason, deleted), "other")}, "no event SuccessfulDrainNode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireFailure(t, requireDrained(t.Context(), managementOf(c.events...), deleted), c.failure)
		})
	}
}

type scaledDown struct {
	deleted, kept *clusterv1.Machine
	machines      []client.Object
	devMachines   []client.Object
	events        []client.Object
	management    []client.Object
	tenant        []client.Object
	workers       *clusterv1.MachineDeployment
	cluster       *clusterv1.Cluster
}

func afterTheScaleDown() *scaledDown {
	deleted, kept := workerMachine("a", "ua", "na"), workerMachine("b", "ub", "nb")
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: scaleKey.Name}}
	cluster.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterAvailableCondition, Status: metav1.ConditionTrue, Reason: "Reason"}}
	one := func() *int32 { return new(int32(1)) }
	workers := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: workersName(scaleKey.Name)},
		Spec:       clusterv1.MachineDeploymentSpec{Replicas: one()},
		Status:     clusterv1.MachineDeploymentStatus{Replicas: one(), ReadyReplicas: one(), AvailableReplicas: one()},
	}
	return &scaledDown{
		deleted:     deleted,
		kept:        kept,
		machines:    []client.Object{kept},
		devMachines: []client.Object{workerDevMachine("b", "docker:///b")},
		events:      []client.Object{workerEvent(drainedReason, deleted)},
		tenant:      []client.Object{workerNode("nb", "docker:///b", corev1.ConditionTrue)},
		workers:     workers,
		cluster:     cluster,
	}
}

func (s *scaledDown) check(ctx context.Context, t *testing.T) error {
	t.Helper()
	var all []client.Object
	for _, group := range [][]client.Object{s.machines, s.devMachines, s.events, s.management, {s.workers, s.cluster}} {
		all = append(all, group...)
	}
	return requireScaledDown(ctx, managementOf(all...), tenantOf(t, s.tenant...), scaleKey, s.deleted, s.kept)
}

func TestAScaleDownIsDoneWhenTheMachineAndEverythingItOwnedAreGoneAndTheOtherStays(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*scaledDown)
		failure string
	}{
		{"everything gone", func(*scaledDown) {}, ""},
		{"the Machine is still there", func(s *scaledDown) { s.machines = append(s.machines, s.deleted) }, "Machine a: still there"},
		{"the Machine is being deleted", func(s *scaledDown) {
			s.deleted.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			s.deleted.Finalizers = []string{clusterv1.MachineFinalizer}
			s.machines = append(s.machines, s.deleted)
		}, "Machine a: still there"},
		{"its DevMachine is still there", func(s *scaledDown) { s.devMachines = append(s.devMachines, workerDevMachine("a", "docker:///a")) }, "DevMachine a: still there"},
		{"the other Machine is gone too", func(s *scaledDown) { s.machines = nil }, "the Machine b that stays"},
		{"the other Machine was replaced under the same name", func(s *scaledDown) { s.machines = []client.Object{workerMachine("b", "replaced", "nb")} }, "has the UID replaced, it had ub"},
		{"the other Machine lost its nodeRef", func(s *scaledDown) { s.machines = []client.Object{workerMachine("b", "ub", "")} }, "its nodeRef is"},
		{"the other Machine is not Running", func(s *scaledDown) {
			m := workerMachine("b", "ub", "nb")
			m.Status.Phase = string(clusterv1.MachinePhaseProvisioned)
			s.machines = []client.Object{m}
		}, "is Provisioned"},
		{"no drain event", func(s *scaledDown) { s.events = nil }, "no event SuccessfulDrainNode"},
		{"the drain event is the other Machine's", func(s *scaledDown) { s.events = []client.Object{workerEvent(drainedReason, s.kept)} }, "no event SuccessfulDrainNode"},
		{"a failed drain", func(s *scaledDown) { s.events = append(s.events, workerEvent(drainFailedReason, s.deleted)) }, "FailedDrainNode"},
		{"a failed deletion of the Node", func(s *scaledDown) { s.events = append(s.events, workerEvent(nodeFailedReason, s.deleted)) }, "FailedDeleteNode"},
		{"the Node is still in the tenant", func(s *scaledDown) {
			s.tenant = append(s.tenant, workerNode("na", "docker:///a", corev1.ConditionFalse))
		}, "Node na: still there"},
		{"the Node is only in the management cluster", func(s *scaledDown) {
			s.management = append(s.management, workerNode("na", "docker:///a", corev1.ConditionTrue))
		}, ""},
		{"two replicas wanted", func(s *scaledDown) { s.workers.Spec.Replicas = new(int32(2)) }, "spec.replicas 2, want 1"},
		{"two replicas counted", func(s *scaledDown) { s.workers.Status.Replicas = new(int32(2)) }, "status.replicas 2, want 1"},
		{"no replicas counted", func(s *scaledDown) { s.workers.Status.Replicas = nil }, "status.replicas unset, want 1"},
		{"the replica is not ready", func(s *scaledDown) { s.workers.Status.ReadyReplicas = new(int32(0)) }, "status.readyReplicas 0, want 1"},
		{"the replica is not available", func(s *scaledDown) { s.workers.Status.AvailableReplicas = new(int32(0)) }, "status.availableReplicas 0, want 1"},
		{"the Cluster is not Available", func(s *scaledDown) { s.cluster.Status.Conditions[0].Status = metav1.ConditionFalse }, "Available is"},
		{"the Cluster has no Available condition", func(s *scaledDown) { s.cluster.Status.Conditions = nil }, "Available is"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := afterTheScaleDown()
			c.change(s)
			requireFailure(t, s.check(t.Context(), t), c.failure)
		})
	}
}

var (
	boundArgumentOf = map[string]int{
		"until":               2,
		"requireUnpaused":     3,
		"whilePaused":         3,
		"pauseCycle":          3,
		"settle":              3,
		"hold":                3,
		"endAfter":            1,
		"context.WithTimeout": 1,
	}
	contextsThatEnd          = []string{"context.WithTimeout", "endAfter"}
	waitsOnTheCallersContext = []string{
		"pollUntil", "apply", "applyRelease", "available", "installCertManager", "deploymentsAvailable", "deploymentAvailableWithin",
		"clusterctlproxy.MoveUntilBlocked",
	}
	waitsOfTheirOwn = []string{
		"docker", "dumpBounded", "dumpTenant", "dumpWorkers", "install", "createWhenWebhooksAnswer", "gatedServer", "returnsWithin",
	}
	waitsWithoutABound = []string{
		"time.Sleep", "time.After", "time.AfterFunc", "time.NewTimer", "time.NewTicker", "time.Tick",
		"context.WithDeadline", "context.WithTimeoutCause", "context.WithDeadlineCause",
	}
	boundsOutsideTheJoinTest = []string{"Install", "Dump"}
)

func calleeOf(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok {
			return pkg.Name + "." + fun.Sel.Name
		}
	}
	return ""
}

func isBoundsField(sel *ast.SelectorExpr) bool {
	owner, ok := sel.X.(*ast.Ident)
	return ok && owner.Name == "bounds"
}

func isAPoll(callee string) bool {
	return strings.HasPrefix(callee, "wait.") && callee != "wait.Interrupted"
}

func isAWait(callee string) bool {
	_, bounded := boundArgumentOf[callee]
	return bounded || slices.Contains(waitsOnTheCallersContext, callee) || slices.Contains(waitsWithoutABound, callee) || isAPoll(callee)
}

// problemsWithTheWaits holds every wait of the Test functions of file to a
// field of bounds that no other wait takes, since total counts each field once,
// and every wait that takes its bound from the context it runs on to a context
// that a field of bounds ends in the same step.
func problemsWithTheWaits(fset *token.FileSet, file *ast.File, fields, outside []string) []string {
	var problems []string
	problem := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
	}
	var waits int
	used := make(map[string]int)
	taken := make(map[*ast.SelectorExpr]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		var path []ast.Node
		ended := make(map[ast.Node]map[string]bool)
		step := func() ast.Node {
			for _, n := range slices.Backward(path) {
				if _, ok := n.(*ast.FuncLit); ok {
					return n
				}
			}
			return nil
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				path = path[:len(path)-1]
				return true
			}
			path = append(path, n)
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Rhs) != 1 {
					break
				}
				call, ok := n.Rhs[0].(*ast.CallExpr)
				if !ok || !slices.Contains(contextsThatEnd, calleeOf(call)) {
					break
				}
				ident, isIdent := n.Lhs[0].(*ast.Ident)
				if bound, isField := argumentOf(call, boundArgumentOf[calleeOf(call)]).(*ast.SelectorExpr); isIdent && isField && isBoundsField(bound) {
					scope := step()
					if ended[scope] == nil {
						ended[scope] = make(map[string]bool)
					}
					ended[scope][ident.Name] = true
				}
			case *ast.CallExpr:
				callee := calleeOf(n)
				at, isWait := boundArgumentOf[callee]
				switch {
				case isWait:
					waits++
					if len(n.Args) <= at {
						problem(n, "%s takes a bound as its argument %d", callee, at+1)
						break
					}
					bound, ok := n.Args[at].(*ast.SelectorExpr)
					if !ok || !isBoundsField(bound) {
						problem(n, "%s is bounded by something that is not a field of bounds", callee)
						break
					}
					taken[bound] = true
					used[bound.Sel.Name]++
				case slices.Contains(waitsOnTheCallersContext, callee):
					waits++
					if ctx, ok := argumentOf(n, 0).(*ast.Ident); !ok || !ended[step()][ctx.Name] {
						problem(n, "%s runs on a context that no field of bounds ends in this step", callee)
					}
				case slices.Contains(waitsWithoutABound, callee) || isAPoll(callee):
					problem(n, "%s waits without a field of bounds to bound it", callee)
				}
			case *ast.SelectorExpr:
				if isBoundsField(n) && !taken[n] {
					problem(n, "bounds.%s is not the bound of a wait that the test knows", n.Sel.Name)
				}
			}
			return true
		})
	}
	if waits == 0 {
		problems = append(problems, "the file holds no wait")
	}
	for _, field := range fields {
		want := 1
		if slices.Contains(outside, field) {
			want = 0
		}
		if used[field] != want {
			problems = append(problems, fmt.Sprintf("bounds.%s bounds %d waits of the file, want %d", field, used[field], want))
		}
	}
	return problems
}

func argumentOf(call *ast.CallExpr, at int) ast.Expr {
	if at >= len(call.Args) {
		return nil
	}
	return call.Args[at]
}

func problemsWithTheFunctionsThatWait(fset *token.FileSet, files []*ast.File) []string {
	known := slices.Concat(slices.Collect(maps.Keys(boundArgumentOf)), waitsOnTheCallersContext, waitsOfTheirOwn)
	var problems []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || strings.HasPrefix(fn.Name.Name, "Test") || slices.Contains(known, fn.Name.Name) {
				continue
			}
			var reported bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && !reported && isAWait(calleeOf(call)) {
					reported = true
					problems = append(problems, fmt.Sprintf("%s: %s calls %s and waits, and the check of the waits does not know it: add it to boundArgumentOf, waitsOnTheCallersContext or waitsOfTheirOwn", fset.Position(fn.Pos()), fn.Name.Name, calleeOf(call)))
				}
				return true
			})
		}
	}
	return problems
}

func fieldsOfTheBounds() []string {
	var names []string
	for field := range reflect.TypeFor[boundSet]().Fields() {
		names = append(names, field.Name)
	}
	return names
}

func TestEveryWaitOfTheJoinTestIsBoundedByAFieldOfTheBoundsThatNoOtherWaitTakes(t *testing.T) {
	for field, bound := range reflect.ValueOf(bounds).Fields() {
		if bound.Int() <= 0 {
			t.Errorf("the bound %s is %s", field.Name, bound)
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "join_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problemsWithTheWaits(fset, file, fieldsOfTheBounds(), boundsOutsideTheJoinTest) {
		t.Error(problem)
	}
}

func TestTheCheckOfTheWaitsRefusesWhatBreaksTheSumOfTheBounds(t *testing.T) {
	call := func(callee string, at int, bound string) string {
		args := make([]string, at+1)
		for i := range args {
			args[i] = "arg"
		}
		args[at] = bound
		return callee + "(" + strings.Join(args, ", ") + ")"
	}
	var all, twice []string
	field := make(map[string]string)
	for _, callee := range slices.Sorted(maps.Keys(boundArgumentOf)) {
		field[callee] = "bounds." + string(rune('A'+len(all)))
		all = append(all, call(callee, boundArgumentOf[callee], field[callee]))
		twice = append(twice, call(callee, boundArgumentOf[callee], "bounds.A"))
	}
	madeBy := func(maker string) string {
		made := slices.Clone(all)
		made[slices.Index(slices.Sorted(maps.Keys(boundArgumentOf)), maker)] = "bounded, cancel := " + call(maker, boundArgumentOf[maker], field[maker])
		return strings.Join(made, "\n")
	}
	fields := []string{"Install"}
	for i := range all {
		fields = append(fields, string(rune('A'+i)))
	}
	cases := []struct {
		name, body, want string
	}{
		{"every wait takes a field of its own", strings.Join(all, "\n"), ""},
		{"nothing waits", "", "holds no wait"},
		{"a field that bounds two waits", strings.Join(twice, "\n"), fmt.Sprintf("bounds.A bounds %d waits", len(twice))},
		{"a field that bounds no wait", strings.Join(all[1:], "\n"), "bounds.A bounds 0 waits"},
		{"the install's bound", strings.Join(all, "\n") + "\n" + call("until", 2, "bounds.Install"), "bounds.Install bounds 1 waits of the file, want 0"},
		{"a bound that is not a wait's", strings.Join(all, "\n") + "\nother(ctx, bounds.Install)", "bounds.Install is not the bound of a wait"},
		{"a field that bounds a wait, written as a sum", strings.Join(all[1:], "\n") + "\nuntil(ctx, t, bounds.A+time.Second)", "is bounded by something that is not a field of bounds"},
		{"a wait without a bound", strings.Join(all, "\n") + "\nuntil(ctx, t)", "until takes a bound as its argument 3"},
		{"a wait with a field of another value", strings.Join(all[1:], "\n") + "\nuntil(ctx, t, other.A)", "is bounded by something that is not a field of bounds"},
		{"a poll of apimachinery's wait package", strings.Join(all, "\n") + "\nwait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, f)", "wait.PollUntilContextTimeout waits without a field"},
		{"apimachinery's test for an interrupted wait", strings.Join(all, "\n") + "\nwait.Interrupted(err)", ""},
	}
	for _, callee := range waitsOnTheCallersContext {
		cases = append(cases,
			struct{ name, body, want string }{callee + " on the step's own context", strings.Join(all, "\n") + "\n" + callee + "(ctx)", callee + " runs on a context that no field of bounds ends in this step"},
			struct{ name, body, want string }{callee + " on a context made in another step", "func() {\n" + madeBy("context.WithTimeout") + "\n}()\nfunc() {\n" + callee + "(bounded)\n}()", callee + " runs on a context that no field of bounds ends in this step"},
			struct{ name, body, want string }{callee + " on a context made from a literal", strings.Join(all[1:], "\n") + "\nbounded, cancel := context.WithTimeout(ctx, 5*time.Minute)\n" + callee + "(bounded)", callee + " runs on a context that no field of bounds ends in this step"},
		)
		for _, maker := range contextsThatEnd {
			cases = append(cases, struct{ name, body, want string }{callee + " on a context made by " + maker, madeBy(maker) + "\n" + callee + "(bounded)", ""})
		}
	}
	for _, callee := range slices.Sorted(maps.Keys(boundArgumentOf)) {
		at := boundArgumentOf[callee]
		var others []string
		for other, otherAt := range boundArgumentOf {
			if other != callee {
				others = append(others, call(other, otherAt, "bounds."+string(rune('B'+len(others)))))
			}
		}
		cases = append(cases, struct{ name, body, want string }{
			callee + " bounded by a literal", call(callee, at, "5*time.Minute") + "\n" + strings.Join(others, "\n"), callee + " is bounded by something that is not a field of bounds",
		})
	}
	for _, callee := range []string{
		"time.Sleep", "time.After", "time.AfterFunc", "time.NewTimer", "time.NewTicker", "time.Tick",
		"context.WithDeadline", "context.WithTimeoutCause", "context.WithDeadlineCause",
	} {
		cases = append(cases, struct{ name, body, want string }{callee, strings.Join(all, "\n") + "\n" + callee + "()", callee + " waits without a field"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := "package p\nfunc TestX() {\n" + c.body + "\n}\nfunc until() { wait.PollUntilContextTimeout(); time.Sleep(); bounds.Install() }\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheWaits(fset, file, fields, []string{"Install"}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestTheCheckOfTheFunctionsThatWaitRefusesWhatItDoesNotKnow(t *testing.T) {
	cases := []struct {
		name, source, want string
	}{
		{"a helper that polls", "func helper() { wait.PollUntilContextCancel() }", "helper calls wait.PollUntilContextCancel"},
		{"a helper that sleeps", "func helper() { time.Sleep() }", "helper calls time.Sleep"},
		{"a helper that makes a ticker in a loop", "func helper() { for { time.NewTicker() } }", "helper calls time.NewTicker"},
		{"a helper that makes a context with a timeout", "func helper() { context.WithTimeout() }", "helper calls context.WithTimeout"},
		{"a helper that ends a context", "func helper() { endAfter() }", "helper calls endAfter"},
		{"a helper that calls a wait that takes its bound", "func helper() { settle() }", "helper calls settle"},
		{"a helper that calls a wait on its caller's context", "func helper() { pollUntil() }", "helper calls pollUntil"},
		{"a helper that calls a wait of clusterctlproxy", "func helper() { clusterctlproxy.MoveUntilBlocked() }", "helper calls clusterctlproxy.MoveUntilBlocked"},
		{"a method that waits", "func (r *r) helper() { time.After() }", "helper calls time.After"},
		{"a helper that waits in a closure", "func helper() { f := func() { time.After() }; f() }", "helper calls time.After"},
		{"a wait that takes its bound", "func until() { wait.PollUntilContextCancel() }", ""},
		{"a wait on its caller's context", "func pollUntil() { wait.PollUntilContextCancel() }", ""},
		{"a wait of its own", "func docker() { context.WithTimeout() }", ""},
		{"a Test function", "func TestX() { time.Sleep() }", ""},
		{"a helper that does not wait", "func helper() { time.Now(); time.Since(); time.Unix(); wait.Interrupted() }", ""},
		{"a declaration without a body", "func helper()", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package p\n"+c.source+"\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(problemsWithTheFunctionsThatWait(fset, []*ast.File{file}), "\n")
			switch {
			case c.want == "" && problems != "":
				t.Fatalf("want accepted, got %s", problems)
			case c.want != "" && !strings.Contains(problems, c.want):
				t.Fatalf("want a problem with %q, got %q", c.want, problems)
			}
		})
	}
}

func TestEveryFunctionOfThePackageThatWaitsIsOneThatTheCheckOfTheWaitsKnows(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	declared := make(map[string]bool)
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				declared[fn.Name.Name] = true
			}
		}
	}
	for _, problem := range problemsWithTheFunctionsThatWait(fset, files) {
		t.Error(problem)
	}
	known := slices.Concat(slices.Collect(maps.Keys(boundArgumentOf)), waitsOnTheCallersContext, waitsOfTheirOwn)
	for _, name := range known {
		if !strings.Contains(name, ".") && !declared[name] {
			t.Errorf("the check of the waits knows %s, which the package does not declare", name)
		}
	}
}
