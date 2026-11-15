// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	klient "sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

func nextWorkersName(cluster string) string { return cluster + "-md-1" }

func raiseTheVersion(ctx context.Context, k8s client.Client, key client.ObjectKey, version string) error {
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	return k8s.Patch(ctx, lcp, client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"version":%q}}`, version)))
}

func createAll(ctx context.Context, c client.Client, manifest []byte) error {
	return decoder.DecodeEach(ctx, bytes.NewReader(manifest), func(ctx context.Context, obj klient.Object) error {
		return c.Create(ctx, obj)
	})
}

func machinesOf(ctx context.Context, k8s client.Reader, key client.ObjectKey, deployment string) ([]clusterv1.Machine, error) {
	machines := &clusterv1.MachineList{}
	err := k8s.List(ctx, machines, client.InNamespace(key.Namespace), client.MatchingLabels{
		clusterv1.ClusterNameLabel:           key.Name,
		clusterv1.MachineDeploymentNameLabel: deployment,
	})
	return machines.Items, err
}

func runningWorkersOf(ctx context.Context, k8s client.Reader, key client.ObjectKey, deployment string, want int) ([]clusterv1.Machine, error) {
	machines, err := machinesOf(ctx, k8s, key, deployment)
	if err != nil {
		return nil, err
	}
	return requireRunning(machines, want)
}

func requireTheMachineKept(ctx context.Context, k8s client.Reader, key client.ObjectKey, deployment string, kept *clusterv1.Machine) error {
	workers, err := runningWorkersOf(ctx, k8s, key, deployment, 1)
	if err != nil {
		return err
	}
	if m := workers[0]; m.UID != kept.UID || m.Status.NodeRef.Name != kept.Status.NodeRef.Name {
		return fmt.Errorf("Machine %s of %s has the UID %s and the nodeRef %s, before the step %s and %s: it was replaced", m.Name, deployment, m.UID, m.Status.NodeRef.Name, kept.UID, kept.Status.NodeRef.Name)
	}
	return nil
}

func requireKubelet(node *corev1.Node, version string) error {
	if got := node.Status.NodeInfo.KubeletVersion; got != version {
		return fmt.Errorf("Node %s runs kubelet %q, want %s", node.Name, got, version)
	}
	return nil
}

func requireControlPlaneAvailable(ctx context.Context, k8s client.Reader, key client.ObjectKey) error {
	lcp := &v1alpha1.LodiaControlPlane{}
	if err := k8s.Get(ctx, key, lcp); err != nil {
		return err
	}
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.AvailableCondition); c == nil || c.Status != metav1.ConditionTrue {
		return fmt.Errorf("the Available of the LodiaControlPlane is %+v", c)
	}
	return nil
}

func holdOf(controlPlane string) string {
	return fmt.Sprintf("LodiaControlPlane %s is upgrading (%q preflight check failed)", controlPlane, clusterv1.MachineSetPreflightCheckControlPlaneIsStable)
}

func heldByTheUpgrade(sets []clusterv1.MachineSet, controlPlane string) string {
	hold := holdOf(controlPlane)
	for _, set := range sets {
		if c := meta.FindStatusCondition(set.Status.Conditions, clusterv1.MachineSetScalingUpCondition); c != nil && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, hold) {
			return c.Message
		}
	}
	return ""
}

func behindTheCopy(cp *lodiav1alpha1.ControlPlane, version string) []string {
	var behind []string
	if k := cp.Status.Kubernetes; k != nil && k.Upgrade != nil {
		behind = append(behind, fmt.Sprintf("status.kubernetes.upgrade at the step %s", k.Upgrade.Step))
	}
	var apiServer, controllerManager, scheduler *lodiav1alpha1.ComponentStatus
	if c := cp.Status.Components; c != nil {
		apiServer, controllerManager, scheduler = c.APIServer, c.ControllerManager, c.Scheduler
	}
	var addons string
	if a := cp.Status.Addons; a != nil {
		addons = a.Version
	}
	versionOf := func(c *lodiav1alpha1.ComponentStatus) string {
		if c == nil {
			return ""
		}
		return c.Version
	}
	for _, part := range []struct{ name, version string }{
		{"status.components.apiServer", versionOf(apiServer)},
		{"status.components.controllerManager", versionOf(controllerManager)},
		{"status.components.scheduler", versionOf(scheduler)},
		{"status.addons", addons},
	} {
		if part.version != version {
			behind = append(behind, fmt.Sprintf("%s.version %q", part.name, part.version))
		}
	}
	return behind
}

func progressingOf(cp *lodiav1alpha1.ControlPlane) string {
	c := meta.FindStatusCondition(cp.Status.Conditions, lodiav1alpha1.ConditionProgressing)
	if c == nil {
		return "Progressing is absent"
	}
	return fmt.Sprintf("Progressing is %s (%s): %s", c.Status, c.Reason, c.Message)
}

func readyOf(lcp *v1alpha1.LodiaControlPlane) string {
	c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition)
	if c == nil {
		return "absent"
	}
	return fmt.Sprintf("%s (%s)", c.Status, c.Reason)
}

type minorStep struct {
	key           client.ObjectKey
	deployment    string
	to            string
	held          string
	violation     error
	readyHistory  []string
	lastReadyRead string
}

func newMinorStep(key client.ObjectKey, to string) *minorStep {
	return &minorStep{key: key, deployment: nextWorkersName(key.Name), to: to}
}

func (s *minorStep) poll(ctx context.Context, k8s client.Reader) error {
	machines, err := machinesOf(ctx, k8s, s.key, s.deployment)
	if err != nil {
		return err
	}
	lcp := &v1alpha1.LodiaControlPlane{}
	if err := k8s.Get(ctx, s.key, lcp); err != nil {
		return err
	}
	cp := &lodiav1alpha1.ControlPlane{}
	if err := k8s.Get(ctx, s.key, cp); err != nil {
		return err
	}
	if ready := readyOf(lcp); ready != s.lastReadyRead {
		s.lastReadyRead = ready
		s.readyHistory = append(s.readyHistory, ready)
	}
	var copied string
	if len(lcp.Status.Versions) > 0 {
		copied = lcp.Status.Versions[0].Version
	}
	behind := behindTheCopy(cp, s.to)
	switch {
	case len(machines) > 0 && copied != s.to:
		s.violation = fmt.Errorf("Machine %s of %s exists, and the LodiaControlPlane read after it reports %q, not %s: the MachineSet made it while the control plane was upgrading", machines[0].Name, s.deployment, copied, s.to)
		return nil
	case copied == s.to && len(behind) > 0:
		s.violation = fmt.Errorf("the LodiaControlPlane reports %s, and Lodia's ControlPlane read after it still has %s", s.to, strings.Join(behind, ", "))
		return nil
	case copied == s.to:
		return nil
	}
	sets := &clusterv1.MachineSetList{}
	if err := k8s.List(ctx, sets, client.InNamespace(s.key.Namespace), client.MatchingLabels{
		clusterv1.ClusterNameLabel:           s.key.Name,
		clusterv1.MachineDeploymentNameLabel: s.deployment,
	}); err != nil {
		return err
	}
	if held := heldByTheUpgrade(sets.Items, s.key.Name); held != "" {
		s.held = held
	}
	return fmt.Errorf("the LodiaControlPlane reports %q, not %s yet; Lodia's ControlPlane is behind in %s, and %s", copied, s.to, strings.Join(behind, ", "), progressingOf(cp))
}

func (s *minorStep) verdict() error {
	switch {
	case s.violation != nil:
		return s.violation
	case s.held == "":
		return fmt.Errorf("the ScalingUp of the MachineSet of %s never said %q while the control plane was upgrading, so nothing shows that Cluster API held its Machine", s.deployment, holdOf(s.key.Name))
	}
	return nil
}

func TestRaisingTheVersionPatchesTheVersionOfTheLodiaControlPlaneAlone(t *testing.T) {
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: scaleKey.Name}}
	lcp.Spec.Version = "v1.36.4"
	lcp.Spec.Datastore.Dedicated = &lodiav1alpha1.DedicatedDatastore{ClassName: "e2e-capi"}
	other := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: "other"}}
	other.Spec.Version = "v1.36.4"
	k8s := managementOf(lcp, other)
	if err := raiseTheVersion(t.Context(), k8s, scaleKey, "v1.37.0"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{scaleKey.Name: "v1.37.0", "other": "v1.36.4"} {
		got := &v1alpha1.LodiaControlPlane{}
		if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: scaleKey.Namespace, Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Version != want {
			t.Errorf("LodiaControlPlane %s has the version %s, want %s", name, got.Spec.Version, want)
		}
	}
	got := &v1alpha1.LodiaControlPlane{}
	if err := k8s.Get(t.Context(), scaleKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Datastore.Dedicated == nil || got.Spec.Datastore.Dedicated.ClassName != "e2e-capi" {
		t.Errorf("the patch changed the rest of the spec: %+v", got.Spec.Datastore)
	}
	if err := raiseTheVersion(t.Context(), managementOf(), scaleKey, "v1.37.0"); err == nil {
		t.Error("a LodiaControlPlane that does not exist was patched")
	}
}

func TestEveryObjectOfAManifestIsCreated(t *testing.T) {
	manifest, err := render("workers-next.yaml", testdataVariables(scaleKey.Name, "v1.37.0"))
	if err != nil {
		t.Fatal(err)
	}
	k8s := managementOf()
	if err := createAll(t.Context(), k8s, manifest); err != nil {
		t.Fatal(err)
	}
	next := &clusterv1.MachineDeployment{}
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: scaleKey.Namespace, Name: nextWorkersName(scaleKey.Name)}, next); err != nil {
		t.Fatal(err)
	}
	if next.Spec.Template.Spec.Version != "v1.37.0" {
		t.Errorf("the MachineDeployment has the version %q, want v1.37.0", next.Spec.Template.Spec.Version)
	}
	if err := createAll(t.Context(), k8s, manifest); err == nil {
		t.Error("a second create of the same objects was accepted")
	}
}

func TestTheTestdataHoldsTheMachineDeploymentThatTheStepOfAMinorCreatesAtTheNextVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "workers-next.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var next *clusterv1.MachineDeployment
	kinds := make(map[string]string)
	for document := range strings.SplitSeq(string(raw), "\n---\n") {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatal(err)
		}
		kinds[object.Kind] = object.Metadata.Name
		if object.Kind == "MachineDeployment" {
			next = &clusterv1.MachineDeployment{}
			if err := yaml.Unmarshal([]byte(document), next); err != nil {
				t.Fatal(err)
			}
		}
	}
	if next == nil {
		t.Fatal("testdata/workers-next.yaml holds no MachineDeployment")
	}
	for kind, name := range kinds {
		if want := nextWorkersName("${CLUSTER_NAME}"); name != want {
			t.Errorf("the %s is named %s, want %s", kind, name, want)
		}
	}
	if next.Spec.Replicas == nil || *next.Spec.Replicas != 1 {
		t.Errorf("the MachineDeployment has %s replicas, want 1", count(next.Spec.Replicas))
	}
	if got := next.Spec.Template.Spec.Version; got != "${KUBERNETES_VERSION}" {
		t.Errorf("the MachineDeployment's version is %q, want the variable the step sets to the version it raises the control plane to", got)
	}
	if got := next.Spec.ClusterName; got != "${CLUSTER_NAME}" {
		t.Errorf("the MachineDeployment's clusterName is %q, want the Cluster's", got)
	}
	for kind, ref := range map[string]string{
		"KubeadmConfigTemplate": next.Spec.Template.Spec.Bootstrap.ConfigRef.Name,
		"DevMachineTemplate":    next.Spec.Template.Spec.InfrastructureRef.Name,
	} {
		if ref != kinds[kind] {
			t.Errorf("the MachineDeployment refers to the %s %q, the file holds %q", kind, ref, kinds[kind])
		}
	}
	if len(kinds) != 3 {
		t.Errorf("the file holds the kinds %v, want a MachineDeployment and the two templates it refers to", kinds)
	}
}

func machineOfDeployment(name, uid, node, deployment string) *clusterv1.Machine {
	m := workerMachine(name, uid, node)
	m.Labels[clusterv1.MachineDeploymentNameLabel] = deployment
	return m
}

func TestTheMachinesOfADeploymentAreTheOnesItsLabelNames(t *testing.T) {
	first, second := nextWorkersName(scaleKey.Name), workersName(scaleKey.Name)
	objects := []client.Object{
		machineOfDeployment("a", "ua", "na", second),
		machineOfDeployment("b", "ub", "nb", first),
		mutated(machineOfDeployment("c", "uc", "nc", first), func(m *clusterv1.Machine) { m.Labels[clusterv1.ClusterNameLabel] = "other" }),
		workerMachine("d", "ud", "nd"),
	}
	k8s := managementOf(objects...)
	for deployment, want := range map[string]string{first: "b", second: "a", "none": ""} {
		machines, err := machinesOf(t.Context(), k8s, scaleKey, deployment)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for _, m := range machines {
			got += m.Name
		}
		if got != want {
			t.Errorf("the Machines of %s are %q, want %q", deployment, got, want)
		}
	}
	_, err := runningWorkersOf(t.Context(), k8s, scaleKey, first, 1)
	requireFailure(t, err, "")
	_, err = runningWorkersOf(t.Context(), k8s, scaleKey, first, 2)
	requireFailure(t, err, "1 Machines, want 2")
	_, err = runningWorkersOf(t.Context(), managementOf(mutated(machineOfDeployment("a", "ua", "", first), func(*clusterv1.Machine) {})), scaleKey, first, 1)
	requireFailure(t, err, "nodeRef")
}

func TestAMachineIsKeptWhenItsDeploymentHoldsItAloneWithTheUIDAndTheNodeRefItHad(t *testing.T) {
	deployment := workersName(scaleKey.Name)
	kept := machineOfDeployment("a", "ua", "na", deployment)
	cases := []struct {
		name    string
		objects []client.Object
		failure string
	}{
		{"the same Machine", []client.Object{kept.DeepCopy()}, ""},
		{"a Machine made again", []client.Object{machineOfDeployment("a", "again", "na", deployment)}, "it was replaced"},
		{"a Machine with another Node", []client.Object{machineOfDeployment("a", "ua", "other", deployment)}, "it was replaced"},
		{"a Machine that does not run", []client.Object{mutated(machineOfDeployment("a", "ua", "na", deployment), func(m *clusterv1.Machine) {
			m.Status.Phase = string(clusterv1.MachinePhaseDeleting)
		})}, "is Deleting"},
		{"a second Machine of the Deployment", []client.Object{kept.DeepCopy(), machineOfDeployment("b", "ub", "nb", deployment)}, "2 Machines, want 1"},
		{"a Machine of another Deployment beside it", []client.Object{kept.DeepCopy(), machineOfDeployment("b", "ub", "nb", nextWorkersName(scaleKey.Name))}, ""},
		{"no Machine", nil, "0 Machines, want 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireFailure(t, requireTheMachineKept(t.Context(), managementOf(c.objects...), scaleKey, deployment, kept), c.failure)
		})
	}
}

func TestAKubeletIsTheVersionWhenTheNodeReportsItExactly(t *testing.T) {
	node := workerNode("n", "p", corev1.ConditionTrue)
	node.Status.NodeInfo.KubeletVersion = "v1.36.4"
	requireFailure(t, requireKubelet(node, "v1.36.4"), "")
	requireFailure(t, requireKubelet(node, "v1.37.0"), `Node n runs kubelet "v1.36.4", want v1.37.0`)
	requireFailure(t, requireKubelet(workerNode("m", "p", corev1.ConditionTrue), "v1.36.4"), `Node m runs kubelet "", want v1.36.4`)
}

func TestTheControlPlaneIsAvailableWhenItsAvailableConditionIsTrue(t *testing.T) {
	lcp := func(status metav1.ConditionStatus) *v1alpha1.LodiaControlPlane {
		l := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: scaleKey.Name}}
		if status != "" {
			l.Status.Conditions = []metav1.Condition{
				{Type: clusterv1.ReadyCondition, Status: metav1.ConditionTrue},
				{Type: clusterv1.AvailableCondition, Status: status},
			}
		}
		return l
	}
	requireFailure(t, requireControlPlaneAvailable(t.Context(), managementOf(lcp(metav1.ConditionTrue)), scaleKey), "")
	requireFailure(t, requireControlPlaneAvailable(t.Context(), managementOf(lcp(metav1.ConditionFalse)), scaleKey), "the Available of the LodiaControlPlane is")
	requireFailure(t, requireControlPlaneAvailable(t.Context(), managementOf(lcp("")), scaleKey), "the Available of the LodiaControlPlane is nil")
	requireFailure(t, requireControlPlaneAvailable(t.Context(), managementOf(), scaleKey), "not found")
}

func scalingUp(status metav1.ConditionStatus, message string) clusterv1.MachineSet {
	return clusterv1.MachineSet{Status: clusterv1.MachineSetStatus{Conditions: []metav1.Condition{
		{Type: clusterv1.MachineSetScalingUpCondition, Status: status, Message: message},
	}}}
}

func TestTheHoldIsTheMessageOfAMachineSetThatIsScalingUpWhileTheControlPlaneIsUpgrading(t *testing.T) {
	const hold = "Scaling up from 0 to 1 replicas:\n* Blocked because LodiaControlPlane w is upgrading (\"ControlPlaneIsStable\" preflight check failed)"
	cases := []struct {
		name string
		sets []clusterv1.MachineSet
		want string
	}{
		{"the hold", []clusterv1.MachineSet{scalingUp(metav1.ConditionTrue, hold)}, hold},
		{"the hold in the second MachineSet", []clusterv1.MachineSet{scalingUp(metav1.ConditionFalse, ""), scalingUp(metav1.ConditionTrue, hold)}, hold},
		{"a MachineSet that scales up with nothing in its way", []clusterv1.MachineSet{scalingUp(metav1.ConditionTrue, "Scaling up from 0 to 1 replicas")}, ""},
		{"the Kubernetes version skew check", []clusterv1.MachineSet{scalingUp(metav1.ConditionTrue, "Scaling up from 0 to 1 replicas:\n* Blocked because MachineSet version (1.37.0) and ControlPlane version (1.36.4) do not conform to the kubernetes version skew policy as MachineSet version is higher than ControlPlane version (\"KubernetesVersionSkew\" preflight check failed)")}, ""},
		{"the hold of another control plane", []clusterv1.MachineSet{scalingUp(metav1.ConditionTrue, strings.Replace(hold, "w is upgrading", "other is upgrading", 1))}, ""},
		{"the hold of a control plane that is provisioning", []clusterv1.MachineSet{scalingUp(metav1.ConditionTrue, strings.Replace(hold, "is upgrading", "is provisioning", 1))}, ""},
		{"the message of a condition that is not True", []clusterv1.MachineSet{scalingUp(metav1.ConditionFalse, hold)}, ""},
		{"a MachineSet with no condition", []clusterv1.MachineSet{{}}, ""},
		{"no MachineSet", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := heldByTheUpgrade(c.sets, "w"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func lodiaAt(version string, change ...func(*lodiav1alpha1.ControlPlaneStatus)) *lodiav1alpha1.ControlPlane {
	part := func() *lodiav1alpha1.ComponentStatus {
		return &lodiav1alpha1.ComponentStatus{Rendered: lodiav1alpha1.Rendered{Version: version}}
	}
	cp := &lodiav1alpha1.ControlPlane{Status: lodiav1alpha1.ControlPlaneStatus{
		Kubernetes: &lodiav1alpha1.KubernetesStatus{Version: version},
		Components: &lodiav1alpha1.ComponentsStatus{APIServer: part(), ControllerManager: part(), Scheduler: part()},
		Addons:     &lodiav1alpha1.Rendered{Version: version},
	}}
	for _, c := range change {
		c(&cp.Status)
	}
	return cp
}

func TestAControlPlaneIsBehindTheCopyWhileAnUpgradeIsReportedOrAPartRunsAnotherVersion(t *testing.T) {
	cases := []struct {
		name   string
		cp     *lodiav1alpha1.ControlPlane
		behind []string
	}{
		{"every part at the version, and no upgrade", lodiaAt("v1.37.0"), nil},
		{"an upgrade at its last step", lodiaAt("v1.37.0", func(s *lodiav1alpha1.ControlPlaneStatus) {
			s.Kubernetes.Upgrade = &lodiav1alpha1.UpgradeStatus{Step: lodiav1alpha1.UpgradeStepAddons}
		}), []string{"status.kubernetes.upgrade at the step Addons"}},
		{"the API server at the version before the rest", lodiaAt("v1.36.4", func(s *lodiav1alpha1.ControlPlaneStatus) {
			s.Components.APIServer.Version = "v1.37.0"
		}), []string{`status.components.controllerManager.version "v1.36.4"`, `status.components.scheduler.version "v1.36.4"`, `status.addons.version "v1.36.4"`}},
		{"the addons alone before the version", lodiaAt("v1.37.0", func(s *lodiav1alpha1.ControlPlaneStatus) { s.Addons.Version = "v1.36.4" }), []string{`status.addons.version "v1.36.4"`}},
		{"the scheduler alone before the version", lodiaAt("v1.37.0", func(s *lodiav1alpha1.ControlPlaneStatus) { s.Components.Scheduler.Version = "" }), []string{`status.components.scheduler.version ""`}},
		{"no part reported", &lodiav1alpha1.ControlPlane{}, []string{
			`status.components.apiServer.version ""`, `status.components.controllerManager.version ""`, `status.components.scheduler.version ""`, `status.addons.version ""`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := behindTheCopy(c.cp, "v1.37.0"); !slices.Equal(got, c.behind) {
				t.Errorf("got %q, want %q", got, c.behind)
			}
		})
	}
}

type minorWorld struct {
	machines          int
	stepped           bool
	finished          bool
	held              bool
	ready, readyCause string
}

const (
	worldFrom = "v1.36.4"
	worldTo   = "v1.37.0"
	never     = 1 << 20
)

type timeline struct{ machineAt, copyAt, finishAt, heldAt, heldUntil int }

func (tl timeline) at(read int) minorWorld {
	w := minorWorld{ready: "Unknown", readyCause: "LatestGenerationNotObserved"}
	if read >= tl.machineAt {
		w.machines = 1
	}
	if read >= tl.copyAt {
		w.stepped, w.ready, w.readyCause = true, "True", "Ready"
	}
	w.finished = read >= tl.finishAt
	w.held = read >= tl.heldAt && read < tl.heldUntil
	return w
}

type scriptedMinor struct {
	from, to string
	world    func(read int) minorWorld
	reads    int
	order    []string
}

func inAWorld(tl timeline) *scriptedMinor {
	return &scriptedMinor{from: worldFrom, to: worldTo, world: tl.at}
}

func (s *scriptedMinor) read(kind string) minorWorld {
	s.reads++
	s.order = append(s.order, kind)
	return s.world(s.reads)
}

func (s *scriptedMinor) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch list := list.(type) {
	case *clusterv1.MachineList:
		for range s.read("Machines").machines {
			list.Items = append(list.Items, *machineOfDeployment("m", "um", "nm", nextWorkersName(scaleKey.Name)))
		}
	case *clusterv1.MachineSetList:
		message := "Scaling up from 0 to 1 replicas"
		if s.world(s.reads).held {
			message += ":\n* Blocked because LodiaControlPlane " + scaleKey.Name + " is upgrading (\"ControlPlaneIsStable\" preflight check failed)"
		}
		list.Items = []clusterv1.MachineSet{scalingUp(metav1.ConditionTrue, message)}
	default:
		return fmt.Errorf("a list of %T is not one of the reads", list)
	}
	return nil
}

func (s *scriptedMinor) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch obj := obj.(type) {
	case *v1alpha1.LodiaControlPlane:
		w := s.read("LodiaControlPlane")
		copied := s.from
		if w.stepped {
			copied = s.to
		}
		obj.Status.Versions = []clusterv1.StatusVersion{{Version: copied}}
		obj.Status.Conditions = []metav1.Condition{{Type: clusterv1.ReadyCondition, Status: metav1.ConditionStatus(w.ready), Reason: w.readyCause}}
	case *lodiav1alpha1.ControlPlane:
		w := s.read("ControlPlane")
		cp := lodiaAt(s.to)
		if !w.finished {
			cp = lodiaAt(s.from, func(status *lodiav1alpha1.ControlPlaneStatus) {
				status.Components.APIServer.Version = s.to
				status.Kubernetes.Upgrade = &lodiav1alpha1.UpgradeStatus{Step: lodiav1alpha1.UpgradeStepControllerManager}
			})
			cp.Status.Conditions = []metav1.Condition{{Type: lodiav1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: "Upgrading", Message: "upgrading to " + s.to + ", step ControllerManager"}}
		}
		obj.Status = cp.Status
	default:
		return fmt.Errorf("a get of %T is not one of the reads", obj)
	}
	return nil
}

func (s *scriptedMinor) run(step *minorStep, polls int) (done int) {
	for done = 1; done <= polls; done++ {
		if step.poll(context.Background(), s) == nil {
			return done
		}
	}
	return 0
}

func TestAPollListsTheMachinesThenReadsTheCopyThenLodiasReport(t *testing.T) {
	s := inAWorld(timeline{never, never, never, 1, never})
	step := newMinorStep(scaleKey, worldTo)
	if err := step.poll(t.Context(), s); err == nil {
		t.Fatal("a poll ended while Lodia has not rolled")
	}
	if want := []string{"Machines", "LodiaControlPlane", "ControlPlane"}; !slices.Equal(s.order, want) {
		t.Fatalf("the poll read %v, want %v", s.order, want)
	}
}

func TestAMinorStepIsHeldOrBrokenAsTheOrderedReadsOfEachPollSayAndTheHoldIsSeenOnce(t *testing.T) {
	cases := []struct {
		name      string
		timeline  timeline
		polls     int
		done      int
		violation string
		held      bool
	}{
		{"a Machine that comes with the copy, and a copy that comes with Lodia", timeline{5, 5, 4, 1, never}, 5, 2, "", true},
		{"a copy that comes at the read in which Lodia finishes", timeline{never, 3, 3, 1, never}, 5, 2, "", true},
		{"a Machine that comes after the copy and Lodia", timeline{10, 5, 4, 1, never}, 5, 2, "", true},
		{"a Machine with the copy still the old version", timeline{2, never, never, 1, never}, 5, 2, "Machine m of w-md-1 exists, and the LodiaControlPlane read after it reports \"v1.36.4\", not v1.37.0", true},
		{"a copy of the new version while Lodia has not rolled", timeline{never, 2, never, 1, never}, 5, 1, "the LodiaControlPlane reports v1.37.0, and Lodia's ControlPlane read after it still has status.kubernetes.upgrade at the step ControllerManager", true},
		{"a step that is never held", timeline{never, 5, 4, never, never}, 5, 2, "", false},
		{"a hold that is gone before the copy moves", timeline{never, 8, 7, 1, 4}, 5, 3, "", true},
		{"a step that does not end", timeline{never, never, never, 1, never}, 5, 0, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := inAWorld(c.timeline)
			step := newMinorStep(scaleKey, worldTo)
			if done := s.run(step, c.polls); done != c.done {
				t.Fatalf("the wait ended at poll %d, want %d", done, c.done)
			}
			err := step.verdict()
			switch {
			case c.violation != "" && (err == nil || !strings.Contains(err.Error(), c.violation)):
				t.Fatalf("want a violation with %q, got %v", c.violation, err)
			case c.violation == "" && c.held && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.violation == "" && !c.held && (err == nil || !strings.Contains(err.Error(), "never said")):
				t.Fatalf("want an error that the hold was never said, got %v", err)
			}
			if c.held && step.held == "" && c.violation == "" && c.done > 0 {
				t.Error("the hold was seen and not kept")
			}
		})
	}
}

func TestAMinorStepIsRunToTheVersionItIsGiven(t *testing.T) {
	s := inAWorld(timeline{5, 5, 4, 1, never})
	s.from, s.to = "v1.37.0", "v1.38.0"
	step := newMinorStep(scaleKey, "v1.38.0")
	if done := s.run(step, 5); done != 2 {
		t.Fatalf("the wait ended at poll %d, want 2", done)
	}
	if err := step.verdict(); err != nil {
		t.Fatal(err)
	}
	s = inAWorld(timeline{5, 5, 4, 1, never})
	step = newMinorStep(scaleKey, "v1.38.0")
	s.run(step, 5)
	requireFailure(t, step.verdict(), `reports "v1.37.0", not v1.38.0`)
}

func TestTheHoldIsReadFromTheMachineSetsOfTheNextDeploymentAlone(t *testing.T) {
	hold := "Scaling up from 0 to 1 replicas:\n* Blocked because " + holdOf(scaleKey.Name)
	machineSet := func(name, deployment, cluster, message string) *clusterv1.MachineSet {
		set := &clusterv1.MachineSet{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: name, Labels: map[string]string{
			clusterv1.ClusterNameLabel:           cluster,
			clusterv1.MachineDeploymentNameLabel: deployment,
		}}}
		set.Status = scalingUp(metav1.ConditionTrue, message).Status
		return set
	}
	lcp := &v1alpha1.LodiaControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: scaleKey.Name}}
	lcp.Status.Versions = []clusterv1.StatusVersion{{Version: worldFrom}}
	cp := lodiaAt(worldFrom)
	cp.ObjectMeta = metav1.ObjectMeta{Namespace: scaleKey.Namespace, Name: scaleKey.Name}
	cases := []struct {
		name string
		sets []client.Object
		want string
	}{
		{"the MachineSet of the next Deployment", []client.Object{machineSet("a", nextWorkersName(scaleKey.Name), scaleKey.Name, hold)}, hold},
		{"the MachineSet of the first Deployment", []client.Object{machineSet("a", workersName(scaleKey.Name), scaleKey.Name, hold)}, ""},
		{"the MachineSet of the next Deployment of another Cluster", []client.Object{machineSet("a", nextWorkersName(scaleKey.Name), "other", hold)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step := newMinorStep(scaleKey, worldTo)
			_ = step.poll(t.Context(), managementOf(append([]client.Object{lcp.DeepCopy(), cp.DeepCopy()}, c.sets...)...))
			if step.held != c.want {
				t.Errorf("the hold is %q, want %q", step.held, c.want)
			}
		})
	}
}

func TestAPollThatEndsNoStepNamesWhatLodiaStillDoes(t *testing.T) {
	s := inAWorld(timeline{never, never, never, 1, never})
	step := newMinorStep(scaleKey, worldTo)
	err := step.poll(t.Context(), s)
	for _, want := range []string{`the LodiaControlPlane reports "v1.36.4", not v1.37.0 yet`, `status.kubernetes.upgrade at the step ControllerManager`, `status.components.controllerManager.version "v1.36.4"`, "Progressing is True (Upgrading): upgrading to v1.37.0, step ControllerManager"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want the error to say %q, got %v", want, err)
		}
	}
}

func TestTheReadyOfTheControlPlaneIsLoggedAsItChangesAndNeverAsserted(t *testing.T) {
	s := inAWorld(timeline{never, 8, 7, 1, never})
	step := newMinorStep(scaleKey, worldTo)
	if done := s.run(step, 5); done != 3 {
		t.Fatalf("the wait ended at poll %d, want 3", done)
	}
	if err := step.verdict(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Unknown (LatestGenerationNotObserved)", "True (Ready)"}; !slices.Equal(step.readyHistory, want) {
		t.Errorf("the Ready history is %q, want %q", step.readyHistory, want)
	}
}

func TestAReadFailureEndsAPollWithItsErrorAndNoVerdict(t *testing.T) {
	s := inAWorld(timeline{never, never, never, 1, never})
	step := newMinorStep(scaleKey, worldTo)
	failing := &failingMinor{scriptedMinor: s, failAt: "ControlPlane"}
	if err := step.poll(t.Context(), failing); !errors.Is(err, errRead) {
		t.Fatalf("got %v, want the read's error", err)
	}
	if step.violation != nil || step.held != "" {
		t.Errorf("a failed read left a violation %v or a hold %q", step.violation, step.held)
	}
}

var errRead = errors.New("the API server did not answer")

type failingMinor struct {
	*scriptedMinor
	failAt string
}

func (f *failingMinor) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*lodiav1alpha1.ControlPlane); ok && f.failAt == "ControlPlane" {
		return errRead
	}
	return f.scriptedMinor.Get(ctx, key, obj, opts...)
}
