// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Command components reads the rendered config/default on its standard input
// and fails unless:
//   - every variable it references has a non-empty default, no default
//     references a variable, and a later reference of a variable has the same
//     default as its first;
//   - it holds one Namespace, and every object of a namespaced kind is in it;
//   - every object has the label cluster.x-k8s.io/provider;
//   - a Deployment has a container named manager;
//   - the roles that reach the manager's ServiceAccount are one Role, in the
//     manager's namespace, and one ClusterRole, and every binding to either
//     names the ServiceAccount alone;
//   - the Role holds the rules of leader election and no other: create and
//     patch on events, create on leases, and get and update on the Lease that
//     the manager's LeaderElectionID names;
//   - a ClusterRoleBinding gives the manager's ServiceAccount a ClusterRole
//     that the render holds and that has no aggregationRule and no rule on
//     leases or events;
//   - a ClusterRoleBinding gives the User of the kubeadm bootstrap provider's
//     ServiceAccount, as the variables LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE
//     and LODIA_KUBEADM_BOOTSTRAP_SA_NAME name it, a ClusterRole that grants
//     get on lodiacontrolplanes and nothing else;
//   - no binding names a subject but the manager's ServiceAccount, by name and
//     namespace, and that User.
//
// The first four checks read the render as written, since clusterctl's
// processing would hide what they look for: it fills in the defaults, moves
// every namespaced object into the namespace it installs in, adds a Namespace
// when there is none, and adds the provider label:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L196-L209
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L250-L260
// The Namespace, label and manager container are rules of what clusterctl asks
// of a components YAML:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md#components-yaml
//
// The roles are checked on the objects clusterctl installs, which are what
// repository.NewComponents makes of the render, once with every variable at its
// default and once with an explicit value for each; the checks hold on both.
// NewComponents parses every image and refuses the ko:// reference that ko
// resolves in a release, so a fixed digest reference takes its place first:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L217-L223
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/container/image.go#L44-L48
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

const (
	providerLabel = "cluster.x-k8s.io/provider"

	serviceAccountUserPrefix  = "system:serviceaccount:"
	serviceAccountsGroup      = "system:serviceaccounts"
	serviceAccountGroupPrefix = serviceAccountsGroup + ":"
	authenticatedGroup        = "system:authenticated"

	// The ServiceAccount of the kubeadm bootstrap provider as Cluster API
	// installs it, which the defaults of the variables name:
	// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/default/kustomization.yaml#L1-L4
	// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/rbac/service_account.yaml#L1-L4
	bootstrapNamespaceVariable = "LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE"
	bootstrapAccountVariable   = "LODIA_KUBEADM_BOOTSTRAP_SA_NAME"
	bootstrapNamespace         = "capi-kubeadm-bootstrap-system"
	bootstrapAccount           = "capi-kubeadm-bootstrap-manager"

	defaultsRun = "every variable at its default"
	explicitRun = "an explicit value for every variable"

	pinnedImage = "example.invalid/cluster-api-provider-lodia@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var (
	koReference = regexp.MustCompile(`ko://[^\s"']+`)
	processor   = yamlprocessor.NewSimpleProcessor()
)

// clusterScoped is clusterctl's list of the kinds it leaves outside the target
// namespace. It is internal to Cluster API, so this is a copy:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/internal/util/objs.go#L81-L103
var clusterScoped = []string{
	"Namespace",
	"Node",
	"PersistentVolume",
	"PodSecurityPolicy",
	"CertificateSigningRequest",
	"ClusterRoleBinding",
	"ClusterRole",
	"VolumeAttachment",
	"StorageClass",
	"CSIDriver",
	"CSINode",
	"ValidatingWebhookConfiguration",
	"MutatingWebhookConfiguration",
	"CustomResourceDefinition",
	"PriorityClass",
	"RuntimeClass",
}

func main() {
	if err := check(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(r io.Reader) error {
	options, err := controller.ManagerOptions(controller.Config{})
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	objs, err := decodeObjects(raw)
	if err != nil {
		return err
	}
	errs := []error{requireContract(objs)}
	variables, err := processor.GetVariableMap(raw)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("clusterctl cannot read the variables of config/default: %w", err))...)
	}
	explicit := make(map[string]string, len(variables))
	for _, name := range slices.Sorted(maps.Keys(variables)) {
		errs = append(errs, requireDefault(name, variables[name]))
		explicit[name] = "explicit-" + strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	}
	errs = append(errs, requireOneDefault(raw, variables))
	for _, name := range []string{bootstrapNamespaceVariable, bootstrapAccountVariable} {
		if _, ok := variables[name]; !ok {
			errs = append(errs, fmt.Errorf("config/default does not reference the variable %s, which names the kubeadm bootstrap provider's ServiceAccount", name))
		}
	}
	for _, run := range []struct {
		name, bootstrapUser string
		values              map[string]string
	}{
		{defaultsRun, serviceAccountUserPrefix + bootstrapNamespace + ":" + bootstrapAccount, nil},
		{explicitRun, serviceAccountUserPrefix + explicit[bootstrapNamespaceVariable] + ":" + explicit[bootstrapAccountVariable], explicit},
	} {
		if err := requireRoles(raw, run.values, options.LeaderElectionID, run.bootstrapUser); err != nil {
			errs = append(errs, fmt.Errorf("with %s: %w", run.name, err))
		}
	}
	return errors.Join(errs...)
}

func decodeObjects(raw []byte) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
			return objs, nil
		} else if err != nil {
			return nil, err
		}
		if len(obj.Object) == 0 {
			continue
		}
		objs = append(objs, obj)
	}
}

func managersOf(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	var managers []*unstructured.Unstructured
	for _, obj := range objs {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		if slices.ContainsFunc(containers, func(c any) bool {
			container, ok := c.(map[string]any)
			return ok && container["name"] == "manager"
		}) {
			managers = append(managers, obj)
		}
	}
	return managers
}

func requireContract(objs []*unstructured.Unstructured) error {
	var errs []error
	var namespaces []string
	for _, obj := range objs {
		if _, ok := obj.GetLabels()[providerLabel]; !ok {
			errs = append(errs, fmt.Errorf("%s %s has no label %s", obj.GetKind(), obj.GetName(), providerLabel))
		}
		if obj.GetKind() == "Namespace" {
			namespaces = append(namespaces, obj.GetName())
		}
	}
	if len(namespaces) != 1 {
		errs = append(errs, fmt.Errorf("config/default renders %d Namespaces, clusterctl wants one", len(namespaces)))
	} else {
		for _, obj := range objs {
			if !slices.Contains(clusterScoped, obj.GetKind()) && obj.GetNamespace() != namespaces[0] {
				errs = append(errs, fmt.Errorf("%s %s is in the namespace %q, not in the Namespace %s: the contract wants every namespaced object in it", obj.GetKind(), obj.GetName(), obj.GetNamespace(), namespaces[0]))
			}
		}
	}
	if len(managersOf(objs)) == 0 {
		errs = append(errs, errors.New("config/default renders no Deployment with a container named manager, which clusterctl looks for"))
	}
	return errors.Join(errs...)
}

// clusterctl's parser reads a variable inside a default as text, not as a
// variable, and reports nil for no default and for an empty one.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/yamlprocessor/simple_processor.go#L76-L93
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/yamlprocessor/simple_processor.go#L159-L190
func requireDefault(name string, def *string) error {
	if def == nil {
		return fmt.Errorf("the variable %s has no default, or an empty one", name)
	}
	nested, err := processor.GetVariableMap([]byte(*def))
	if err != nil {
		return fmt.Errorf("the default of the variable %s: %w", name, err)
	}
	if len(nested) > 0 {
		return fmt.Errorf("the default of the variable %s references the variables %v", name, slices.Sorted(maps.Keys(nested)))
	}
	return nil
}

// clusterctl reports the default of a variable from its first reference only,
// and expands a later reference by its own, so a bare reference after one with a
// default installs an empty value, and one with another default installs that.
// A render with no value for any variable, which expands each reference by its
// own default, must equal the render that gives every variable the default
// clusterctl reports. This cannot see ${X#a}, ${X%a} and ${X:1}, whose argument
// clusterctl records as a default and which both runs expand to nothing; no
// kustomize render writes them.
// A variable with no default makes Process fail, which requireDefault reports.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/yamlprocessor/simple_processor.go#L159-L190
func requireOneDefault(raw []byte, variables map[string]*string) error {
	unset := func(string) (string, error) { return "", errors.New("unset") }
	atDefaults := func(name string) (string, error) {
		if def := variables[name]; def != nil {
			return *def, nil
		}
		return "", errors.New("unset")
	}
	own, ownErr := processor.Process(raw, unset)
	first, firstErr := processor.Process(raw, atDefaults)
	if ownErr == nil && firstErr == nil && !bytes.Equal(own, first) {
		return errors.New("a variable is referenced again without its default or with another one: clusterctl reports the default of its first reference, and expands each later one by its own")
	}
	return nil
}

func requireRoles(raw []byte, values map[string]string, lease, bootstrapUser string) error {
	objs, err := install(raw, values)
	if err != nil {
		return fmt.Errorf("clusterctl cannot process config/default: %w", err)
	}
	access, err := readRBAC(objs)
	if err != nil {
		return err
	}
	var errs []error
	managers := managersOf(objs)
	for _, manager := range managers {
		errs = append(errs, requireManagerRBAC(access, manager, lease))
	}
	return errors.Join(append(errs, requireSubjects(access, managers, bootstrapUser), requireBootstrapGrant(access, bootstrapUser))...)
}

func install(raw []byte, values map[string]string) ([]*unstructured.Unstructured, error) {
	configClient, err := config.New(context.Background(), "", config.InjectReader(config.NewMemoryReader()))
	if err != nil {
		return nil, err
	}
	for name, value := range values {
		configClient.Variables().Set(name, value)
	}
	components, err := repository.NewComponents(repository.ComponentsInput{
		Provider:     config.NewProvider("lodia", "https://example.invalid", clusterctlv1.ControlPlaneProviderType),
		ConfigClient: configClient,
		Processor:    processor,
		RawYaml:      koReference.ReplaceAll(raw, []byte(pinnedImage)),
	})
	if err != nil {
		return nil, err
	}
	objs := components.Objs()
	installed := make([]*unstructured.Unstructured, len(objs))
	for i := range objs {
		installed[i] = &objs[i]
	}
	return installed, nil
}

type roleID struct{ kind, namespace, name string }

func (r roleID) String() string {
	if r.namespace == "" {
		return r.kind + " " + r.name
	}
	return r.kind + " " + r.namespace + "/" + r.name
}

type rolePolicy struct {
	rules       []rbacv1.PolicyRule
	aggregation *rbacv1.AggregationRule
}

type roleGrant struct {
	kind, name string
	role       roleID
	subjects   []rbacv1.Subject
}

type access struct {
	roles  map[roleID]rolePolicy
	grants []roleGrant
}

// The API server reads a key by exact case, and clusterctl creates objects
// without field validation, which the API server defaults to Warn: a key like
// resourcenames is dropped with a warning, and the rule grants more. So an
// unknown field is an error here.
func decode(obj *unstructured.Unstructured, into any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(obj.Object, into, true); err != nil {
		return fmt.Errorf("decode %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

func readRBAC(objs []*unstructured.Unstructured) (access, error) {
	a := access{roles: make(map[roleID]rolePolicy)}
	var errs []error
	for _, obj := range objs {
		var err error
		switch obj.GetKind() {
		case "Role":
			var r rbacv1.Role
			if err = decode(obj, &r); err == nil {
				a.roles[roleID{"Role", obj.GetNamespace(), obj.GetName()}] = rolePolicy{rules: r.Rules}
			}
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err = decode(obj, &r); err == nil {
				a.roles[roleID{"ClusterRole", "", obj.GetName()}] = rolePolicy{rules: r.Rules, aggregation: r.AggregationRule}
			}
		case "RoleBinding":
			var b rbacv1.RoleBinding
			if err = decode(obj, &b); err == nil {
				ref := roleID{b.RoleRef.Kind, "", b.RoleRef.Name}
				if ref.kind == "Role" {
					ref.namespace = obj.GetNamespace()
				}
				a.grants = append(a.grants, roleGrant{obj.GetKind(), obj.GetName(), ref, b.Subjects})
			}
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			if err = decode(obj, &b); err == nil {
				a.grants = append(a.grants, roleGrant{obj.GetKind(), obj.GetName(), roleID{b.RoleRef.Kind, "", b.RoleRef.Name}, b.Subjects})
			}
		}
		errs = append(errs, err)
	}
	return a, errors.Join(errs...)
}

func requireManagerRBAC(a access, manager *unstructured.Unstructured, lease string) error {
	ns := manager.GetNamespace()
	account := serviceAccount(manager)
	who := fmt.Sprintf("the ServiceAccount %q of Deployment %s", account, manager.GetName())
	var reaching, roles, clusterRoles []roleID
	for _, g := range a.grants {
		if g.reaches(account) && !slices.Contains(reaching, g.role) {
			reaching = append(reaching, g.role)
			switch g.role.kind {
			case "Role":
				roles = append(roles, g.role)
			case "ClusterRole":
				clusterRoles = append(clusterRoles, g.role)
			}
		}
	}
	var errs []error
	switch {
	case len(roles) != 1:
		errs = append(errs, fmt.Errorf("%s reaches the Roles %v, want the leader election Role in %s alone", who, roles, ns))
	case !a.has(roles[0]):
		errs = append(errs, fmt.Errorf("%s has a RoleBinding to %s, which config/default does not render", who, roles[0]))
	default:
		if diff := cmp.Diff(leaderElectionRules(lease), a.roles[roles[0]].rules, cmpopts.EquateEmpty()); diff != "" {
			errs = append(errs, fmt.Errorf("the rules of %s are not the leader election rules (-want +got):\n%s", roles[0], diff))
		}
	}
	switch {
	case len(clusterRoles) != 1:
		errs = append(errs, fmt.Errorf("%s reaches the ClusterRoles %v, want the manager's ClusterRole alone", who, clusterRoles))
	case !a.has(clusterRoles[0]):
		errs = append(errs, fmt.Errorf("%s has a binding to %s, which config/default does not render", who, clusterRoles[0]))
	case !a.clusterBound(clusterRoles[0], account):
		errs = append(errs, fmt.Errorf("%s has no ClusterRoleBinding to %s: the manager could not read a Cluster or a LodiaControlPlane", who, clusterRoles[0]))
	default:
		errs = append(errs, requireNoLeasesOrEvents(clusterRoles[0], a.roles[clusterRoles[0]]))
	}
	alone := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: account, Namespace: ns}}
	for _, g := range a.grants {
		if slices.Contains(reaching, g.role) && !slices.Equal(g.subjects, alone) {
			errs = append(errs, fmt.Errorf("%s %s binds %s to %+v, want %s alone", g.kind, g.name, g.role, g.subjects, who))
		}
	}
	return errors.Join(errs...)
}

func requireSubjects(a access, managers []*unstructured.Unstructured, bootstrapUser string) error {
	var errs []error
	for _, g := range a.grants {
		for _, s := range g.subjects {
			if !isManagerOrBootstrapUser(s, managers, bootstrapUser) {
				errs = append(errs, fmt.Errorf("%s %s binds %s to %+v: a binding may name only the manager's ServiceAccount, by name and namespace, and the User %s", g.kind, g.name, g.role, s, bootstrapUser))
			}
		}
	}
	return errors.Join(errs...)
}

func isManagerOrBootstrapUser(s rbacv1.Subject, managers []*unstructured.Unstructured, bootstrapUser string) bool {
	switch s.Kind {
	case rbacv1.UserKind:
		return s.Name == bootstrapUser
	case rbacv1.ServiceAccountKind:
		return slices.ContainsFunc(managers, func(m *unstructured.Unstructured) bool {
			return s == rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount(m), Namespace: m.GetNamespace()}
		})
	}
	return false
}

func requireBootstrapGrant(a access, bootstrapUser string) error {
	var errs []error
	var roles []roleID
	for _, g := range a.grants {
		if !slices.ContainsFunc(g.subjects, func(s rbacv1.Subject) bool { return s.Kind == rbacv1.UserKind && s.Name == bootstrapUser }) {
			continue
		}
		if g.kind != "ClusterRoleBinding" {
			errs = append(errs, fmt.Errorf("%s %s binds %s to the User %s, which grants only in one namespace: the kubeadm bootstrap provider reads a LodiaControlPlane in any", g.kind, g.name, g.role, bootstrapUser))
		}
		if !slices.Contains(roles, g.role) {
			roles = append(roles, g.role)
		}
	}
	switch {
	case len(roles) == 0:
		return errors.Join(append(errs, fmt.Errorf("no binding names the User %s, which is the kubeadm bootstrap provider's ServiceAccount: its joins could not read a LodiaControlPlane", bootstrapUser))...)
	case len(roles) > 1:
		return errors.Join(append(errs, fmt.Errorf("the User %s reaches the roles %v, want one ClusterRole", bootstrapUser, roles))...)
	case roles[0].kind != "ClusterRole" || !a.has(roles[0]):
		return errors.Join(append(errs, fmt.Errorf("the User %s is bound to %s, want a ClusterRole that config/default renders", bootstrapUser, roles[0]))...)
	}
	role := a.roles[roles[0]]
	if role.aggregation != nil {
		errs = append(errs, fmt.Errorf("%s has an aggregationRule, which adds rules that config/default does not render", roles[0]))
	}
	want := []rbacv1.PolicyRule{{APIGroups: []string{"controlplane.lodia.io"}, Resources: []string{"lodiacontrolplanes"}, Verbs: []string{"get"}}}
	if diff := cmp.Diff(want, role.rules, cmpopts.EquateEmpty()); diff != "" {
		errs = append(errs, fmt.Errorf("the rules of %s, which the User %s gets, are not get on lodiacontrolplanes alone (-want +got):\n%s", roles[0], bootstrapUser, diff))
	}
	return errors.Join(errs...)
}

func (a access) has(id roleID) bool {
	_, ok := a.roles[id]
	return ok
}

func (a access) clusterBound(id roleID, account string) bool {
	return slices.ContainsFunc(a.grants, func(g roleGrant) bool {
		return g.kind == "ClusterRoleBinding" && g.role == id && g.reaches(account)
	})
}

// A create request has no name yet to match, so create on leases is a rule of
// its own.
func leaderElectionRules(lease string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{lease}, Verbs: []string{"get", "update"}},
	}
}

func requireNoLeasesOrEvents(id roleID, r rolePolicy) error {
	var errs []error
	if r.aggregation != nil {
		errs = append(errs, fmt.Errorf("%s has an aggregationRule, which adds rules that config/default does not render", id))
	}
	for i, rule := range r.rules {
		for _, c := range []struct{ group, resource string }{
			{"coordination.k8s.io", "leases"},
			{"", "events"},
			{"events.k8s.io", "events"},
		} {
			if covers(rule.APIGroups, c.group) && covers(rule.Resources, c.resource) {
				errs = append(errs, fmt.Errorf("rule %d of %s grants %v on %s in the group %q, where only the leader election Role grants", i, id, rule.Verbs, c.resource, c.group))
			}
		}
	}
	return errors.Join(errs...)
}

func serviceAccount(manager *unstructured.Unstructured) string {
	account, _, _ := unstructured.NestedString(manager.Object, "spec", "template", "spec", "serviceAccountName")
	return account
}

func (g roleGrant) reaches(account string) bool {
	return slices.ContainsFunc(g.subjects, func(s rbacv1.Subject) bool { return subjectReaches(s, account) })
}

// subjectReaches reports whether the RBAC authorizer applies the subject to the
// manager's ServiceAccount: to the user system:serviceaccount:<namespace>:<name>
// in the groups system:serviceaccounts, system:serviceaccounts:<namespace> and
// system:authenticated.
// clusterctl sets the namespace of every subject that has one to the one it
// installs the provider in, which the installer picks, and leaves a subject
// without one as it is. The authorizer reads a ServiceAccount subject without a
// namespace in a RoleBinding as of the binding's namespace. So a ServiceAccount
// subject of the manager's name reaches it in any namespace or in none, and so
// do the user and the namespace group of any namespace.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L384
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/registry/rbac/validation/rule.go#L281-L304
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go#L29-L33
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go#L126-L136
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/authentication/group/authenticated_group_adder.go#L56
func subjectReaches(s rbacv1.Subject, account string) bool {
	switch s.Kind {
	case rbacv1.ServiceAccountKind:
		return s.Name == account
	case rbacv1.UserKind:
		rest, ok := strings.CutPrefix(s.Name, serviceAccountUserPrefix)
		ns, name, found := strings.Cut(rest, ":")
		return ok && found && ns != "" && name == account
	case rbacv1.GroupKind:
		ns, namespaced := strings.CutPrefix(s.Name, serviceAccountGroupPrefix)
		return s.Name == serviceAccountsGroup || s.Name == authenticatedGroup || namespaced && ns != ""
	}
	return false
}

func covers(list []string, name string) bool {
	return slices.Contains(list, name) || slices.Contains(list, rbacv1.ResourceAll)
}
