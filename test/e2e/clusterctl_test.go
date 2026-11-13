// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/rest"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/test/clusterctlproxy"
)

const (
	certManagerWait    = 2 * time.Minute
	providerWait       = 2 * time.Minute
	developmentPatch   = 99
	overridesFolderKey = "overridesFolder"
)

type clusterctlProvider struct {
	name       string
	kind       clusterctlv1.ProviderType
	version    string
	components []byte
	metadata   []byte
}

// writeRepository lays the providers out as test/framework/clusterctl's
// CreateRepository does, with the directory of overrides empty so that a
// developer's own never applies, and returns the clusterctl configuration for
// it. cert-manager is the suite's own, so clusterctl only waits for it.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/framework/clusterctl/repository.go#L104-L158
func writeRepository(dir string, providers []clusterctlProvider, variables map[string]string) (string, error) {
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	overrides := filepath.Join(dir, "overrides")
	if err := os.MkdirAll(overrides, 0o750); err != nil {
		return "", err
	}
	configured := make([]map[string]string, 0, len(providers))
	for _, p := range providers {
		label := clusterctlv1.ManifestLabel(p.name, p.kind)
		versionDir := filepath.Join(dir, label, p.version)
		if err := os.MkdirAll(versionDir, 0o750); err != nil {
			return "", err
		}
		for file, data := range map[string][]byte{"components.yaml": p.components, "metadata.yaml": p.metadata} {
			if err := os.WriteFile(filepath.Join(versionDir, file), data, 0o600); err != nil {
				return "", err
			}
		}
		configured = append(configured, map[string]string{"name": p.name, "type": string(p.kind), "url": "file://" + filepath.Join(dir, label, "latest", "components.yaml")})
	}
	values := map[string]any{
		"providers":        configured,
		overridesFolderKey: overrides,
		"cert-manager":     map[string]string{"externallyProvisioned": "true", "timeout": certManagerWait.String()},
	}
	for name, value := range variables {
		values[name] = value
	}
	data, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, clusterctlConfigFile)
	return path, os.WriteFile(path, data, 0o600)
}

// developmentVersion is the version under which the tree's own components go
// into the repository: the newest series of its metadata.yaml, with a patch no
// release has.
func developmentVersion(metadata []byte) (string, error) {
	var m clusterctlv1.Metadata
	if err := yaml.Unmarshal(metadata, &m); err != nil {
		return "", err
	}
	if len(m.ReleaseSeries) == 0 {
		return "", errors.New("metadata.yaml has no release series")
	}
	newest := slices.MaxFunc(m.ReleaseSeries, func(a, b clusterctlv1.ReleaseSeries) int {
		if a.Major != b.Major {
			return int(a.Major - b.Major)
		}
		return int(a.Minor - b.Minor)
	})
	return fmt.Sprintf("v%d.%d.%d", newest.Major, newest.Minor, developmentPatch), nil
}

// clusterctl reads a variable from the environment before its configuration
// file, for every component alike, Cluster API's EXP_* and CAPD's among them,
// and counts a variable set to nothing as set. The folder of overrides is read
// the same way, and a components.yaml or metadata.yaml in the one that the
// environment names, or in the default one that an empty variable falls back
// to, replaces the repository's.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/config/reader_viper.go#L82-L88
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/overrides.go#L70-L90
func requireNoClusterctlSettingsInEnvironment(environ []string, components ...[]byte) error {
	set := make(map[string]bool, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		set[name] = true
	}
	var errs []error
	refuse := func(variable string) {
		if set[strings.ToUpper(variable)] {
			errs = append(errs, fmt.Errorf("%s is set in the environment, which clusterctl reads before its configuration and so changes what the suite installs", strings.ToUpper(variable)))
		}
	}
	refuse(overridesFolderKey)
	for _, raw := range components {
		variables, err := yamlprocessor.NewSimpleProcessor().GetVariables(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, variable := range variables {
			refuse(variable)
		}
	}
	return errors.Join(errs...)
}

func newClusterctl(ctx context.Context, cfg *envconf.Config, configPath string) (clusterctl.Client, error) {
	configClient, err := config.New(ctx, configPath)
	if err != nil {
		return nil, err
	}
	return clusterctlproxy.New(ctx, configClient, cfg.Client().RESTConfig(), clientRequestTimeout, discoveryTimeout)
}

// A ServiceAccount's token authenticates as its user, in the groups of every
// ServiceAccount, of its namespace and of every authenticated user, and a
// binding to any of them grants it the same.
func accessReview(deployment *appsv1.Deployment, verb string) *authorizationv1.SubjectAccessReview {
	account := deployment.Spec.Template.Spec.ServiceAccountName
	return &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   serviceaccount.MakeUsername(deployment.Namespace, account),
		Groups: append(serviceaccount.MakeGroupNames(deployment.Namespace), user.AllAuthenticated),
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace,
			Group:     "controlplane.lodia.io",
			Resource:  "lodiacontrolplanes",
			Verb:      verb,
		},
	}}
}

func requireOnlyGet(ctx context.Context, c client.Client, deployment *appsv1.Deployment) error {
	var errs []error
	for _, want := range []struct {
		verb    string
		allowed bool
	}{{"get", true}, {"update", false}} {
		review := accessReview(deployment, want.verb)
		if err := c.Create(ctx, review); err != nil {
			return err
		}
		if review.Status.Allowed != want.allowed {
			errs = append(errs, fmt.Errorf("%s may %s lodiacontrolplanes: %t, want %t (%s)", review.Spec.User, want.verb, review.Status.Allowed, want.allowed, review.Status.Reason))
		}
	}
	return errors.Join(errs...)
}

const clusterctlConfigFile = "clusterctl-config.yaml"

const (
	capiMetadata = "releaseSeries:\n- {major: 1, minor: 15, contract: v1beta2}\n"
	lodiaSeries  = "releaseSeries:\n- {major: 0, minor: 1, contract: v1beta2}\n"
	metadataHead = "apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3\nkind: Metadata\n"
)

func componentsIn(ns string) []byte {
	return []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns + "\n---\n" +
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: manager\n  namespace: " + ns + "\nspec:\n  template:\n    spec:\n      containers:\n      - name: manager\n        image: example.invalid/manager:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n---\n" +
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: b\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: r\nsubjects:\n- apiGroup: rbac.authorization.k8s.io\n  kind: User\n  name: system:serviceaccount:${SA_NAMESPACE:=default-namespace}:${SA_NAME:=default-name}\n")
}

func TestTheRepositoryIsWhatClusterctlReadsBack(t *testing.T) {
	providers := []clusterctlProvider{
		{"cluster-api", clusterctlv1.CoreProviderType, "v1.15.0-beta.0", componentsIn("capi-system"), []byte(metadataHead + capiMetadata)},
		{"kubeadm", clusterctlv1.BootstrapProviderType, "v1.15.0-beta.0", componentsIn("bootstrap-system"), []byte(metadataHead + capiMetadata)},
		{"docker", clusterctlv1.InfrastructureProviderType, "v1.15.0-beta.0", componentsIn("capd-system"), []byte(metadataHead + capiMetadata)},
		{"lodia", clusterctlv1.ControlPlaneProviderType, "v0.1.99", componentsIn("lodia-system"), []byte(metadataHead + lodiaSeries)},
	}
	dir := filepath.Join(t.TempDir(), "repository")
	stale := filepath.Join(dir, "control-plane-lodia", "v0.9.99")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	path, err := writeRepository(dir, providers, map[string]string{"SA_NAMESPACE": "bootstrap", "SA_NAME": "manager"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a version of an earlier run is left in the repository: %v", err)
	}
	configClient, err := config.New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := clusterctlproxy.New(t.Context(), configClient, &rest.Config{Host: "https://never-asked.invalid"}, clientRequestTimeout, discoveryTimeout)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		components, err := c.GetProviderComponents(t.Context(), p.name, p.kind, clusterctl.ComponentsOptions{})
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if components.Version() != p.version {
			t.Errorf("%s: latest is %s, want %s", p.name, components.Version(), p.version)
		}
		for _, obj := range components.Objs() {
			if obj.GetKind() != "ClusterRoleBinding" {
				continue
			}
			subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
			if got := subjects[0].(map[string]any)["name"]; got != "system:serviceaccount:bootstrap:manager" {
				t.Errorf("%s: the subject is %v, want the variables of the configuration to have filled it in", p.name, got)
			}
		}
	}
	certManager, err := configClient.CertManager().Get()
	if err != nil {
		t.Fatal(err)
	}
	if !certManager.ExternallyProvisioned() || certManager.Timeout() != certManagerWait.String() {
		t.Errorf("cert-manager is externally provisioned: %t, its wait is %s, want true and %s", certManager.ExternallyProvisioned(), certManager.Timeout(), certManagerWait)
	}
	overrides, err := os.ReadDir(filepath.Join(dir, "overrides"))
	if err != nil || len(overrides) > 0 {
		t.Errorf("the directory of overrides must exist and be empty, got %v and %v", overrides, err)
	}
	if got, ok := configClient.Variables().Get("SA_NAME"); ok != nil || got != "manager" {
		t.Errorf("the variable SA_NAME reads %q and %v from the configuration", got, ok)
	}
	if got, ok := configClient.Variables().Get(overridesFolderKey); ok != nil || got != filepath.Join(dir, "overrides") {
		t.Errorf("the folder of overrides reads %q and %v from the configuration, want the empty one beside the repository", got, ok)
	}
}

func TestTheDevelopmentVersionIsTheNewestSeriesWithAPatchNoReleaseHas(t *testing.T) {
	cases := []struct {
		name, metadata, want, wantErr string
	}{
		{"one series", metadataHead + lodiaSeries, "v0.1.99", ""},
		{"the newest minor of two, listed first", metadataHead + "releaseSeries:\n- {major: 0, minor: 2, contract: v1beta2}\n- {major: 0, minor: 1, contract: v1beta2}\n", "v0.2.99", ""},
		{"the newest minor of two, listed last", metadataHead + "releaseSeries:\n- {major: 0, minor: 1, contract: v1beta2}\n- {major: 0, minor: 2, contract: v1beta2}\n", "v0.2.99", ""},
		{"a newer major with an older minor", metadataHead + "releaseSeries:\n- {major: 1, minor: 0, contract: v1beta2}\n- {major: 0, minor: 9, contract: v1beta2}\n", "v1.0.99", ""},
		{"no series", metadataHead, "", "no release series"},
		{"not metadata", "a: [\n", "", "yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := developmentVersion([]byte(c.metadata))
			switch {
			case c.wantErr == "" && (err != nil || got != c.want):
				t.Fatalf("got %q and %v, want %q", got, err, c.want)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want an error with %q, got %q and %v", c.wantErr, got, err)
			}
		})
	}
	metadata, err := os.ReadFile(filepath.Join(root, "metadata.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := developmentVersion(metadata); err != nil || got != "v0.1.99" {
		t.Errorf("the tree's metadata.yaml gives %q and %v, want v0.1.99", got, err)
	}
}

func TestAVariableOfAComponentInTheEnvironmentIsRefused(t *testing.T) {
	component := func(variable string) []byte {
		return []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata:\n  k: ${" + variable + ":=d}\n")
	}
	components := [][]byte{component("EXP_CORE"), component("EXP_BOOTSTRAP"), component("CAPD_DOCKER_HOST"), component("LODIA_KUBEADM_BOOTSTRAP_SA_NAME")}
	clean := []string{"PATH=/bin", "HOME=/home/x", "EXP_CORE_NOT=1", "X=EXP_CORE", "LODIA_KUBEADM_BOOTSTRAP_SA=a"}
	if err := requireNoClusterctlSettingsInEnvironment(clean, components...); err != nil {
		t.Fatalf("want an environment without the variables accepted, got %v", err)
	}
	for _, variable := range []string{"EXP_CORE", "EXP_BOOTSTRAP", "CAPD_DOCKER_HOST", "LODIA_KUBEADM_BOOTSTRAP_SA_NAME"} {
		for _, kv := range []string{variable + "=true", variable + "="} {
			err := requireNoClusterctlSettingsInEnvironment(append(slices.Clone(clean), kv), components...)
			if err == nil || !strings.Contains(err.Error(), variable+" is set in the environment") {
				t.Errorf("with %q set: want %s refused, got %v", kv, variable, err)
			}
		}
	}
	if err := requireNoClusterctlSettingsInEnvironment(append(slices.Clone(clean), "EXP_CORE=1", "CAPD_DOCKER_HOST=x"), components...); err == nil || !strings.Contains(err.Error(), "EXP_CORE") || !strings.Contains(err.Error(), "CAPD_DOCKER_HOST") {
		t.Errorf("want both variables named, got %v", err)
	}
	if err := requireNoClusterctlSettingsInEnvironment([]string{"EXP_CORE=1"}, []byte("a: ${X")); err == nil {
		t.Error("a component clusterctl cannot read is accepted")
	}
}

func TestTheFolderOfOverridesInTheEnvironmentIsRefused(t *testing.T) {
	for _, kv := range []string{"OVERRIDESFOLDER=/from/env", "OVERRIDESFOLDER="} {
		if err := requireNoClusterctlSettingsInEnvironment([]string{"PATH=/bin", kv}); err == nil || !strings.Contains(err.Error(), "OVERRIDESFOLDER is set in the environment") {
			t.Errorf("with %q set: want the folder of overrides refused, got %v", kv, err)
		}
	}
	if err := requireNoClusterctlSettingsInEnvironment([]string{"PATH=/bin", "OVERRIDES=/x", "OVERRIDESFOLDER_NOT=1"}); err != nil {
		t.Errorf("want an environment without the variable accepted, got %v", err)
	}
}

func bootstrapDeployment() *appsv1.Deployment {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "bootstrap-system", Name: "controller-manager"}}
	deployment.Spec.Template.Spec.ServiceAccountName = "bootstrap-manager"
	return deployment
}

type grantedTo struct {
	subject string
	verbs   []string
}

func apiServerGranting(t *testing.T, grants ...grantedTo) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			review := obj.(*authorizationv1.SubjectAccessReview)
			if attrs := review.Spec.ResourceAttributes; attrs == nil || attrs.Group != "controlplane.lodia.io" || attrs.Resource != "lodiacontrolplanes" {
				t.Errorf("the review asks about %+v, want lodiacontrolplanes of controlplane.lodia.io", attrs)
			}
			who := append([]string{review.Spec.User}, review.Spec.Groups...)
			for _, g := range grants {
				if slices.Contains(who, g.subject) && slices.Contains(g.verbs, review.Spec.ResourceAttributes.Verb) {
					review.Status.Allowed = true
				}
			}
			return nil
		},
	}).Build()
}

func TestTheGrantOfTheInstalledKubeadmBootstrapProviderIsGetAlone(t *testing.T) {
	const installed = "system:serviceaccount:bootstrap-system:bootstrap-manager"
	cases := []struct {
		name   string
		grants []grantedTo
		want   string
	}{
		{"get to the installed ServiceAccount's user", []grantedTo{{installed, []string{"get"}}}, ""},
		{"get and update to the user", []grantedTo{{installed, []string{"get", "update"}}}, "may update lodiacontrolplanes: true"},
		{"get to the user and update to every authenticated user", []grantedTo{{installed, []string{"get"}}, {"system:authenticated", []string{"update"}}}, "may update lodiacontrolplanes: true"},
		{"get to the user and update to the namespace's ServiceAccounts", []grantedTo{{installed, []string{"get"}}, {"system:serviceaccounts:bootstrap-system", []string{"update"}}}, "may update lodiacontrolplanes: true"},
		{"get to the user and update to every ServiceAccount", []grantedTo{{installed, []string{"get"}}, {"system:serviceaccounts", []string{"update"}}}, "may update lodiacontrolplanes: true"},
		{"get to the default ServiceAccount of the template, which is not the installed one", []grantedTo{{"system:serviceaccount:capi-kubeadm-bootstrap-system:capi-kubeadm-bootstrap-manager", []string{"get"}}}, "may get lodiacontrolplanes: false"},
		{"get to the provider's own ServiceAccount, where clusterctl rewrote the binding", []grantedTo{{"system:serviceaccount:cluster-api-provider-lodia-system:bootstrap-manager", []string{"get"}}}, "may get lodiacontrolplanes: false"},
		{"no grant", nil, "may get lodiacontrolplanes: false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireOnlyGet(t.Context(), apiServerGranting(t, c.grants...), bootstrapDeployment())
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestTheReviewNamesTheUserAndTheGroupsOfTheInstalledServiceAccount(t *testing.T) {
	review := accessReview(bootstrapDeployment(), "get")
	if review.Spec.User != "system:serviceaccount:bootstrap-system:bootstrap-manager" {
		t.Errorf("the user is %q", review.Spec.User)
	}
	if want := []string{"system:serviceaccounts", "system:serviceaccounts:bootstrap-system", "system:authenticated"}; !slices.Equal(review.Spec.Groups, want) {
		t.Errorf("the groups are %v, want %v", review.Spec.Groups, want)
	}
	if review.Spec.ResourceAttributes.Verb != "get" || review.Spec.ResourceAttributes.Namespace != namespace {
		t.Errorf("the attributes are %+v", review.Spec.ResourceAttributes)
	}
}

func TestTheClusterctlClientEndsItsCallsAtTheSuitesTimeouts(t *testing.T) {
	const (
		request = 5 * time.Second
		bound   = time.Second
		slack   = time.Second
	)
	shortenTheDiscovery(t, 500*time.Millisecond)
	shortenTheClientRequests(t, request)
	empty := filepath.Join(t.TempDir(), "clusterctl.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(context.Context, clusterctl.Client) error{
		"Init": func(ctx context.Context, c clusterctl.Client) error {
			_, err := c.Init(ctx, clusterctl.InitOptions{CoreProvider: "cluster-api", ControlPlaneProviders: []string{"lodia"}})
			return err
		},
		"Move": func(ctx context.Context, c clusterctl.Client) error {
			return c.Move(ctx, clusterctl.MoveOptions{ToKubeconfig: clusterctlproxy.Kubeconfig, Namespace: namespace})
		},
	}
	for name, stalled := range stalledTenants {
		for call, run := range calls {
			t.Run(name+"/"+call, func(t *testing.T) {
				url, _ := stalled(t)
				c, err := newClusterctl(t.Context(), configFor(t, url), empty)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), bound)
				defer cancel()
				start := time.Now()
				if err := returnsWithin(t, 2*request, call, func() error { return run(ctx, c) }); err == nil {
					t.Fatalf("%s says nothing about a cluster that did not answer", call)
				}
				if took := time.Since(start); took > bound+discoveryTimeout+slack {
					t.Errorf("%s ended after %s, more than its bound %s and the discovery timeout %s (and %s of slack) after it", call, took, bound, discoveryTimeout, slack)
				}
			})
		}
	}
}

func clusterThatHoldsTheInventory(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			respond(t, w, `{"major":"1","minor":"35","gitVersion":"v1.35.0"}`)
		case "/api":
			respond(t, w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "/apis":
			respond(t, w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apiextensions.k8s.io","versions":[{"groupVersion":"apiextensions.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apiextensions.k8s.io/v1","version":"v1"}}]}`)
		case "/apis/apiextensions.k8s.io/v1":
			respond(t, w, `{"kind":"APIResourceList","groupVersion":"apiextensions.k8s.io/v1","resources":[{"name":"customresourcedefinitions","kind":"CustomResourceDefinition","namespaced":false,"verbs":["get"]}]}`)
		default:
			<-r.Context().Done()
		}
	})
}

func TestTheClusterctlClientsRequestsAskForTheirOwnTimeouts(t *testing.T) {
	shortenTheDiscovery(t, 7*time.Second)
	shortenTheClientRequests(t, 11*time.Second)
	empty := filepath.Join(t.TempDir(), "clusterctl.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	url, asked := recordedServer(t, clusterThatHoldsTheInventory(t))
	c, err := newClusterctl(t.Context(), configFor(t, url), empty)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = returnsWithin(t, 30*time.Second, "Init", func() error {
		_, err := c.Init(ctx, clusterctl.InitOptions{CoreProvider: "cluster-api"})
		return err
	})
	if err == nil {
		t.Fatal("Init says nothing about a cluster that holds the inventory's CustomResourceDefinition")
	}
	for _, suffix := range []string{"/version", "/api", "/apis", "/apis/apiextensions.k8s.io/v1"} {
		requireRequestTimeout(t, asked(), suffix, discoveryTimeout)
	}
	requireRequestTimeout(t, asked(), "/customresourcedefinitions/providers.clusterctl.cluster.x-k8s.io", clientRequestTimeout)
}

const templatedFile = "/etc/capl-e2e/control-plane-version"

func TestTheWorkersTemplateHoldsAFileRenderedFromTheControlPlanesVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "workers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			t.Fatalf("workers.yaml has no KubeadmConfigTemplate: %v", err)
		}
		if obj.GetKind() != "KubeadmConfigTemplate" {
			continue
		}
		files, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "files")
		if len(files) != 1 {
			t.Fatalf("the KubeadmConfigTemplate has %d files, want 1", len(files))
		}
		file := files[0].(map[string]any)
		if file["path"] != templatedFile || file["contentFormat"] != "Template" || file["content"] != "{{ .controlPlane.version }}" {
			t.Fatalf("the file is %v, want %s with contentFormat Template and the control plane's version as content", file, templatedFile)
		}
		return
	}
}
