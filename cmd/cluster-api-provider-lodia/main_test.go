// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	goflag "flag"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	logsv1 "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	ctrl "sigs.k8s.io/controller-runtime"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

type group string

const (
	works   group = "works"
	partly  group = "works in part"
	crashes group = "crashes the manager"
	probes  group = "breaks the probes"
	ignored group = "ignored by the operator"
)

var readmeHeadings = map[group]string{
	works:   "Fields that work",
	partly:  "A field that works only as `:9440`",
	crashes: "Fields that make the manager exit at start",
	probes:  "Fields that break the probes",
	ignored: "Fields the operator ignores",
}

const severalInstances = "Several instances of the provider from the released YAML are not supported."

type reached struct {
	manager   ctrl.Options
	workers   int
	filter    string
	verbosity uint32
}

func (r reached) tls() *tls.Config {
	cfg := &tls.Config{}
	for _, opt := range r.manager.Metrics.TLSOpts {
		opt(cfg)
	}
	return cfg
}

// The fields of spec.manager and the arguments cluster-api-operator's customizer writes for each, copied from
// https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/component_customizer.go#L198-L421
// and its test's output
// https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/component_customizer_test.go#L498-L571
// The operator's CRD requires every field of leaderElection
// https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/config/crd/bases/operator.cluster.x-k8s.io_controlplaneproviders.yaml#L3032-L3039
// so the rows of leaderElect, leaseDuration, renewDeadline and retryPeriod hold what the customizer writes with resourceName and
// resourceNamespace empty, the row of resourceName with both set, and the row of resourceNamespace with resourceName empty.
var managerFields = []struct {
	field  string
	group  group
	args   []string
	option string
	got    func(reached) any
	want   any
	probe  string
}{
	{"controller.groupKindConcurrency", works, []string{"--lodiacontrolplane-concurrency=5"}, "the controller's MaxConcurrentReconciles", func(r reached) any { return r.workers }, 5, ""},
	{"cacheNamespace", works, []string{"--namespace=ns"}, "the cache's DefaultNamespaces", func(r reached) any { return slices.Sorted(maps.Keys(r.manager.Cache.DefaultNamespaces)) }, []string{"ns"}, ""},
	{"metrics.diagnosticsAddress", works, []string{"--diagnostics-address=:9443"}, "the metrics server's BindAddress", func(r reached) any { return r.manager.Metrics.BindAddress }, ":9443", ""},
	{"metrics.insecureDiagnostics", works, []string{"--insecure-diagnostics=true"}, "the metrics server's SecureServing", func(r reached) any { return r.manager.Metrics.SecureServing }, false, ""},
	{"profilerAddress", works, []string{"--profiler-address=localhost:6060"}, "PprofBindAddress", func(r reached) any { return r.manager.PprofBindAddress }, "localhost:6060", ""},
	{"leaderElection.leaderElect", works, []string{"--leader-elect=true"}, "LeaderElection", func(r reached) any { return r.manager.LeaderElection }, true, ""},
	{"leaderElection.leaseDuration", works, []string{"--leader-elect=true", "--leader-elect-lease-duration=25200s"}, "LeaseDuration", func(r reached) any { return deref(r.manager.LeaseDuration) }, 7 * time.Hour, ""},
	{"leaderElection.renewDeadline", works, []string{"--leader-elect=true", "--leader-elect-renew-deadline=25200s"}, "RenewDeadline", func(r reached) any { return deref(r.manager.RenewDeadline) }, 7 * time.Hour, ""},
	{"leaderElection.retryPeriod", works, []string{"--leader-elect=true", "--leader-elect-retry-period=25200s"}, "RetryPeriod", func(r reached) any { return deref(r.manager.RetryPeriod) }, 7 * time.Hour, ""},
	{"syncPeriod", works, []string{"--sync-period=25200s"}, "the cache's SyncPeriod", func(r reached) any { return deref(r.manager.Cache.SyncPeriod) }, 7 * time.Hour, ""},
	{"verbosity", works, []string{"--v=5"}, "the logging configuration's Verbosity", func(r reached) any { return r.verbosity }, uint32(5), ""},
	{"verbosity", works, nil, "the logging configuration's Verbosity, which the field's default of 1 leaves at 2", func(r reached) any { return r.verbosity }, uint32(2), ""},
	{"additionalArgs", works, []string{"--watch-filter=mine"}, "the watch filter's value", func(r reached) any { return r.filter }, "mine", ""},
	{"additionalArgs", works, []string{"--tls-min-version=VersionTLS13"}, "the metrics server's minimum TLS version", func(r reached) any { return r.tls().MinVersion }, uint16(tls.VersionTLS13), ""},
	{"additionalArgs", works, []string{"--tls-cipher-suites=TLS_AES_128_GCM_SHA256"}, "the metrics server's cipher suites", func(r reached) any { return r.tls().CipherSuites }, []uint16{tls.TLS_AES_128_GCM_SHA256}, ""},
	{"additionalArgs", works, []string{"--tls-curve-preferences=23"}, "the metrics server's curves", func(r reached) any { return r.tls().CurvePreferences }, []tls.CurveID{tls.CurveP256}, ""},

	{"health.healthProbeBindAddress", partly, []string{"--health-addr=:9440"}, "HealthProbeBindAddress", func(r reached) any { return r.manager.HealthProbeBindAddress }, ":9440", ""},

	{"controller.groupKindConcurrency", crashes, []string{"--lodiacontrolplane.controlplane.lodia.io-concurrency=5"}, "", nil, nil, ""},
	{"maxConcurrentReconciles", crashes, []string{"--max-concurrent-reconciles=5"}, "", nil, nil, ""},
	{"metrics.bindAddress", crashes, []string{"--metrics-bind-addr=:8080"}, "", nil, nil, ""},
	{"webhook.port", crashes, []string{"--webhook-port=9443"}, "", nil, nil, ""},
	{"webhook.host", crashes, []string{"--webhook-host=0.0.0.0"}, "", nil, nil, ""},
	{"webhook.certDir", crashes, []string{"--webhook-cert-dir=/tmp/certs"}, "", nil, nil, ""},
	{"featureGates", crashes, []string{"--feature-gates=ANOTHER=false,TEST=true"}, "", nil, nil, ""},
	{"leaderElection.resourceName", crashes, []string{"--leader-elect=true", "--leader-election-id=here/foo"}, "", nil, nil, ""},

	{"health.livenessEndpointName", probes, nil, "", nil, nil, "/mostly"},
	{"health.readinessEndpointName", probes, nil, "", nil, nil, "/readyish"},

	{"gracefulShutDown", ignored, nil, "", nil, nil, ""},
	{"controller.cacheSyncTimeout", ignored, nil, "", nil, nil, ""},
	{"controller.recoverPanic", ignored, nil, "", nil, nil, ""},
	{"leaderElection.resourceLock", ignored, nil, "", nil, nil, ""},
	{"leaderElection.resourceNamespace", ignored, nil, "", nil, nil, ""},
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func newFlagSet() *pflag.FlagSet {
	logOptions = logsv1.NewLoggingConfiguration()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(args ...string) error {
	fs := newFlagSet()
	if err := prepare(fs); err != nil {
		return err
	}
	return fs.Parse(args)
}

func loaded(t *testing.T, args ...string) {
	t.Helper()
	t.Cleanup(func() {
		if err := logsv1.ResetForTest(nil); err != nil {
			t.Error(err)
		}
	})
	if err := load(newFlagSet(), args); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
}

func parsed(t *testing.T, args ...string) reached {
	t.Helper()
	if err := parse(args...); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	manager, err := controller.ManagerOptions(config)
	if err != nil {
		t.Fatal(err)
	}
	return reached{
		manager:   manager,
		workers:   controller.ControllerOptions(config).MaxConcurrentReconciles,
		filter:    config.WatchFilterValue,
		verbosity: uint32(logOptions.Verbosity),
	}
}

func TestTheFieldsThatWorkParseAndReachTheOptionTheyName(t *testing.T) {
	for _, f := range managerFields {
		if f.group != works && f.group != partly {
			continue
		}
		t.Run(f.field+" "+strings.Join(f.args, " "), func(t *testing.T) {
			if diff := cmp.Diff(f.want, f.got(parsed(t, f.args...))); diff != "" {
				t.Errorf("%s (-want +got):\n%s", f.option, diff)
			}
		})
	}
}

func TestTheFieldsWhoseFlagIsNotDefinedAreRefused(t *testing.T) {
	for _, f := range managerFields {
		if f.group != crashes {
			continue
		}
		t.Run(f.field+" "+strings.Join(f.args, " "), func(t *testing.T) {
			if err := parse(f.args...); err == nil || !strings.Contains(err.Error(), "unknown flag") {
				t.Errorf("parsing %v returned %v, want an unknown flag", f.args, err)
			}
		})
	}
}

func TestTheFlagsOfTheOldBinaryAreGone(t *testing.T) {
	for _, arg := range []string{"--metrics-bind-address=:8443", "--health-probe-bind-address=:8081", "--zap-devel", "--zap-log-level=info", "--zap-encoder=json"} {
		if err := parse(arg); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("parsing %s returned %v, want an unknown flag", arg, err)
		}
	}
}

func TestTheBinaryReadsUnderscoresAsHyphensAndTakesControllerRuntimesKubeconfigFlag(t *testing.T) {
	t.Run("an underscore for a hyphen", func(t *testing.T) {
		if r := parsed(t, "--leader_elect=true"); !r.manager.LeaderElection {
			t.Error("--leader_elect=true did not turn leader election on")
		}
	})
	t.Run("--kubeconfig", func(t *testing.T) {
		kubeconfig := goflag.Lookup("kubeconfig")
		if kubeconfig == nil {
			t.Fatal("controller-runtime registers no kubeconfig flag in Go's flag set")
		}
		t.Cleanup(func() { _ = kubeconfig.Value.Set(kubeconfig.DefValue) })
		parsed(t, "--kubeconfig=/nowhere")
		if got := kubeconfig.Value.String(); got != "/nowhere" {
			t.Errorf("--kubeconfig reached Go's flag set as %q, want /nowhere", got)
		}
	})
}

func TestTheFlagsDefaultToClusterAPIsValues(t *testing.T) {
	r := parsed(t)
	for _, c := range []struct {
		option    string
		got, want any
	}{
		{"LeaderElection", r.manager.LeaderElection, false},
		{"LeaseDuration", deref(r.manager.LeaseDuration), time.Minute},
		{"RenewDeadline", deref(r.manager.RenewDeadline), 40 * time.Second},
		{"RetryPeriod", deref(r.manager.RetryPeriod), 5 * time.Second},
		{"the cache's SyncPeriod", deref(r.manager.Cache.SyncPeriod), 10 * time.Minute},
		{"the cache's DefaultNamespaces", len(r.manager.Cache.DefaultNamespaces), 0},
		{"HealthProbeBindAddress", r.manager.HealthProbeBindAddress, ":9440"},
		{"PprofBindAddress", r.manager.PprofBindAddress, ""},
		{"the metrics server's BindAddress", r.manager.Metrics.BindAddress, ":8443"},
		{"the metrics server's SecureServing", r.manager.Metrics.SecureServing, true},
		{"the diagnostics endpoint's handlers", slices.Sorted(maps.Keys(r.manager.Metrics.ExtraHandlers)), []string{"/debug/flags/v", "/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/profile", "/debug/pprof/symbol", "/debug/pprof/trace"}},
		{"the metrics server's minimum TLS version", r.tls().MinVersion, uint16(tls.VersionTLS12)},
		{"the controller's MaxConcurrentReconciles", r.workers, 10},
		{"the watch filter's value", r.filter, ""},
		{"the logging configuration's Verbosity", r.verbosity, uint32(2)},
	} {
		if diff := cmp.Diff(c.want, c.got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.option, diff)
		}
	}
}

func TestTheFieldsThatWriteNoFlagMoveTheProbesOffThePathsTheManagerServes(t *testing.T) {
	container := managerContainer(t)
	served := []string{container.LivenessProbe.HTTPGet.Path, container.ReadinessProbe.HTTPGet.Path}
	if !slices.Equal(served, []string{"/healthz", "/readyz"}) {
		t.Fatalf("the manifest's probes use %v, want the paths controller-runtime serves", served)
	}
	options := parsed(t).manager
	if options.LivenessEndpointName != "" || options.ReadinessEndpointName != "" {
		t.Errorf("the manager serves its probes at %q and %q, not at controller-runtime's paths", options.LivenessEndpointName, options.ReadinessEndpointName)
	}
	for _, f := range managerFields {
		if f.group == probes && slices.Contains(served, f.probe) {
			t.Errorf("%s writes the probe path %s, which the manager serves", f.field, f.probe)
		}
	}
}

func TestTheManifestsArgsParseAndItsPortsFollowTheDefaults(t *testing.T) {
	container := managerContainer(t)
	r := parsed(t, container.Args...)
	if !r.manager.LeaderElection {
		t.Error("the manifest's args do not turn leader election on")
	}
	if m := r.manager.Metrics; m.BindAddress != ":8443" || !m.SecureServing {
		t.Errorf("the manifest's defaults serve the metrics on %q with SecureServing %t, want :8443 and true", m.BindAddress, m.SecureServing)
	}
	for _, p := range []struct {
		name, address string
	}{
		{"healthz", r.manager.HealthProbeBindAddress},
		{"metrics", r.manager.Metrics.BindAddress},
	} {
		_, port, err := net.SplitHostPort(p.address)
		if err != nil {
			t.Fatal(err)
		}
		var declared []string
		for _, c := range container.Ports {
			declared = append(declared, c.Name+"="+strconv.Itoa(int(c.ContainerPort)))
		}
		if !slices.Contains(declared, p.name+"="+port) {
			t.Errorf("the manager serves %s on port %s, the manifest declares %v", p.name, port, declared)
		}
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe} {
		if probe.HTTPGet.Port.String() != "healthz" {
			t.Errorf("a probe uses the port %s, not the named port healthz", probe.HTTPGet.Port.String())
		}
	}
}

func managerContainer(t *testing.T) corev1.Container {
	t.Helper()
	return managerContainerWith(t, nil)
}

func managerManifest(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func managerContainerWith(t *testing.T, values map[string]string) corev1.Container {
	t.Helper()
	processed, err := yamlprocessor.NewSimpleProcessor().Process(managerManifest(t), func(name string) (string, error) {
		if value, ok := values[name]; ok {
			return value, nil
		}
		return "", errors.New(name + " is not set")
	})
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(processed), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() != "Deployment" {
			continue
		}
		deployment := &appsv1.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(obj.Object, deployment, true); err != nil {
			t.Fatal(err)
		}
		for _, c := range deployment.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				return c
			}
		}
		t.Fatal("the Deployment has no container named manager")
	}
}

func TestTheManifestsVariablesReachTheMetricsServer(t *testing.T) {
	variables, err := yamlprocessor.NewSimpleProcessor().GetVariables(managerManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(variables)
	if diff := cmp.Diff([]string{"CAPI_DIAGNOSTICS_ADDRESS", "CAPI_INSECURE_DIAGNOSTICS"}, variables); diff != "" {
		t.Errorf("the manifest's variables (-want +got):\n%s", diff)
	}
	metrics := parsed(t, managerContainerWith(t, map[string]string{
		"CAPI_DIAGNOSTICS_ADDRESS":  "127.0.0.1:9000",
		"CAPI_INSECURE_DIAGNOSTICS": "true",
	}).Args...).manager.Metrics
	if metrics.BindAddress != "127.0.0.1:9000" || metrics.SecureServing {
		t.Errorf("the metrics server binds %q with SecureServing %t, want 127.0.0.1:9000 and false from the variables", metrics.BindAddress, metrics.SecureServing)
	}
}

func TestTheDocsListTheFieldsInTheGroupsOfTheTable(t *testing.T) {
	raw, err := os.ReadFile("../../docs/README.md")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string][]string{}
	heading := ""
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "#"):
			heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
		case strings.HasPrefix(line, "- `"):
			field, _, ok := strings.Cut(line[len("- `"):], "`")
			if !ok {
				t.Fatalf("docs/README.md's item %q has no field in backquotes", line)
			}
			listed[heading] = append(listed[heading], field)
		}
	}
	for g, heading := range readmeHeadings {
		var want []string
		for _, f := range managerFields {
			if f.group == g {
				want = append(want, f.field)
			}
		}
		slices.Sort(want)
		got := slices.Sorted(slices.Values(listed[heading]))
		if diff := cmp.Diff(slices.Compact(want), slices.Compact(got)); diff != "" {
			t.Errorf("docs/README.md's fields under %q differ from the table's, which says the group %q (-table +docs):\n%s", heading, g, diff)
		}
	}
	if !strings.Contains(string(raw), "\n"+severalInstances+"\n") {
		t.Errorf("docs/README.md does not say %q", severalInstances)
	}
}

func TestTheLoggingFlagsReachKlogAndControllerRuntime(t *testing.T) {
	loaded(t, "--v=5", "--logging-format=json")
	if !klog.V(5).Enabled() || klog.V(6).Enabled() {
		t.Errorf("klog logs at V(5) %t and V(6) %t, want --v=5", klog.V(5).Enabled(), klog.V(6).Enabled())
	}
	if !ctrl.Log.V(5).Enabled() {
		t.Error("controller-runtime's logger does not log at V(5)")
	}
}
