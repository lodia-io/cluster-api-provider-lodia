// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lodia.io/lodia/api/crds"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

var (
	k8s          client.Client
	admin        *rest.Config
	asController *rest.Config
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("skipping the LodiaControlPlane controller tests: KUBEBUILDER_ASSETS is not set; run make test")
		os.Exit(0)
	}
	lodiaCRDs, err := lodiaCRDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Lodia's CRDs:", err)
		os.Exit(1)
	}
	capi, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "find Cluster API's CRDs:", err)
		os.Exit(1)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join(strings.TrimSpace(string(capi)), "core", "config", "crd", "bases"),
		},
		CRDs:                  lodiaCRDs,
		ErrorIfCRDPathMissing: true,
		UseExistingCluster:    new(false),
	}
	env.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest:", err)
		os.Exit(1)
	}
	stop := func(what string, err error) {
		fmt.Fprintln(os.Stderr, what+":", err)
		_ = env.Stop()
		os.Exit(1)
	}
	admin = cfg
	if k8s, err = client.New(cfg, client.Options{Scheme: controller.Scheme()}); err != nil {
		stop("client", err)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	if asController, err = boundToTheClusterRole(cfg); err != nil {
		stop("the controller's ClusterRole", err)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop envtest:", err)
	}
	os.Exit(code)
}

func lodiaCRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	files, err := fs.Glob(crds.FS, "*.yaml")
	if err != nil {
		return nil, err
	}
	var out []*apiextensionsv1.CustomResourceDefinition
	for _, f := range files {
		raw, err := crds.FS.ReadFile(f)
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(raw, crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, crd)
	}
	return out, nil
}

func boundToTheClusterRole(cfg *rest.Config) (*rest.Config, error) {
	const user = "cluster-api-provider-lodia"
	ctx := context.Background()
	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		return nil, err
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.UnmarshalStrict(manifest, role); err != nil {
		return nil, err
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: user},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}
	for _, obj := range []client.Object{role, binding} {
		if err := k8s.Create(ctx, obj); err != nil {
			return nil, err
		}
	}
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{Group: lodiav1alpha1.GroupVersion.Group, Resource: "controlplanes", Verb: "watch"},
		}}
		if err := k8s.Create(ctx, review); err != nil {
			return false, err
		}
		return review.Status.Allowed, nil
	})
	if err != nil {
		return nil, err
	}
	controller := rest.CopyConfig(cfg)
	controller.Impersonate = rest.ImpersonationConfig{UserName: user}
	return controller, nil
}

func startManager(t *testing.T, ns string, with ...func(*ctrl.Options)) ctrl.Manager {
	t.Helper()
	return startManagerWith(t, controller.Config{Namespace: ns, Concurrency: 10}, with...)
}

func startManagerWith(t *testing.T, c controller.Config, with ...func(*ctrl.Options)) ctrl.Manager {
	t.Helper()
	forTheTest := func(options *ctrl.Options) {
		options.Cache.ReaderFailOnMissingInformer = true
		options.Metrics = metricsserver.Options{BindAddress: "0"}
		options.Controller = config.Controller{SkipNameValidation: new(true)}
	}
	mgr, err := controller.NewManager(asController, c, append([]func(*ctrl.Options){forTheTest}, with...)...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	return mgr
}

func direct(t *testing.T, funcs interceptor.Funcs) *controller.Reconciler {
	t.Helper()
	c, err := client.NewWithWatch(asController, client.Options{Scheme: controller.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	intercepted := interceptor.NewClient(c, funcs)
	return &controller.Reconciler{Client: intercepted, APIReader: intercepted}
}

func pass(ctx context.Context, r *controller.Reconciler, lcp *v1alpha1.LodiaControlPlane) error {
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp)})
	return err
}

func passes(t *testing.T, r *controller.Reconciler, lcp *v1alpha1.LodiaControlPlane) {
	t.Helper()
	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
}

func untilBlockMove(t *testing.T, r *controller.Reconciler, lcp *v1alpha1.LodiaControlPlane) {
	t.Helper()
	for range 5 {
		if err := pass(t.Context(), r, lcp); err != nil {
			t.Fatal(err)
		}
		get(t, lcp)
		if _, ok := lcp.Annotations[clusterctlv1.BlockMoveAnnotation]; ok {
			return
		}
	}
	t.Fatal("no block-move annotation after five passes")
}

func namespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := k8s.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

func spec() v1alpha1.LodiaControlPlaneSpec {
	return v1alpha1.LodiaControlPlaneSpec{
		Version: "v1.35.8",
		LodiaControlPlaneTemplateResourceSpec: v1alpha1.LodiaControlPlaneTemplateResourceSpec{
			Datastore: lodiav1alpha1.DatastoreChoice{Dedicated: &lodiav1alpha1.DedicatedDatastore{ClassName: "fast"}},
			Endpoint: lodiav1alpha1.Endpoint{
				NodePort: &lodiav1alpha1.NodePortEndpoint{},
				CertSANs: []lodiav1alpha1.Hostname{"api.alpha.example.com"},
			},
			Placement:   &lodiav1alpha1.Placement{PriorityClassName: "tenant-control-planes"},
			Maintenance: &lodiav1alpha1.Maintenance{Window: &lodiav1alpha1.MaintenanceWindow{Begin: "01:00", End: "03:00"}},
			Health:      &lodiav1alpha1.Health{HoldControllerManager: new(false)},
		},
	}
}

var everyOptionalField = map[string]func(*v1alpha1.LodiaControlPlaneTemplateResourceSpec){
	"the Split layout with high availability": func(s *v1alpha1.LodiaControlPlaneTemplateResourceSpec) {
		optionalFieldsOfEveryLayout(s)
		s.HighAvailability = &lodiav1alpha1.HighAvailability{FailureTolerance: "Node"}
		s.APIServer = &lodiav1alpha1.APIServer{Replicas: new(int32(2))}
	},
	"the Compact layout": func(s *v1alpha1.LodiaControlPlaneTemplateResourceSpec) {
		optionalFieldsOfEveryLayout(s)
		s.Layout = lodiav1alpha1.LayoutCompact
	},
}

func optionalFieldsOfEveryLayout(s *v1alpha1.LodiaControlPlaneTemplateResourceSpec) {
	s.ControllerManager = &lodiav1alpha1.Component{ExtraArgs: map[string]string{"concurrent-deployment-syncs": "3"}}
	s.Scheduler = &lodiav1alpha1.Scheduler{ExtraArgs: map[string]string{"kube-api-qps": "100"}}
	s.Addons = &lodiav1alpha1.Addons{CoreDNS: &lodiav1alpha1.CoreDNS{Enabled: new(false), Replicas: new(int32(3))}}
	s.ServiceAccount = &lodiav1alpha1.ServiceAccount{Issuer: "https://issuer.example.com"}
}

func create(t *testing.T, ns, name string, mutate ...func(*clusterv1.Cluster, *v1alpha1.LodiaControlPlane)) (*clusterv1.Cluster, *v1alpha1.LodiaControlPlane) {
	t.Helper()
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork: clusterv1.ClusterNetwork{
				Services:      clusterv1.NetworkRanges{CIDRBlocks: []string{"10.128.0.0/12"}},
				Pods:          clusterv1.NetworkRanges{CIDRBlocks: []string{"192.168.0.0/16"}},
				ServiceDomain: "tenant.local",
			},
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: v1alpha1.GroupVersion.Group, Kind: "LodiaControlPlane", Name: name + "-cp",
			},
		},
	}
	lcp := &v1alpha1.LodiaControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name + "-cp",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: name},
		},
		Spec: spec(),
	}
	for _, m := range mutate {
		m(cluster, lcp)
	}
	if err := k8s.Create(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	lcp.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: cluster.Name, UID: cluster.UID,
	}}
	if err := k8s.Create(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	return cluster, lcp
}

type ca struct{ cert, key []byte }

func ownedBy(cp *lodiav1alpha1.ControlPlane) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(cp, lodiav1alpha1.GroupVersion.WithKind("ControlPlane"))}
}

func lodiaSecrets(t *testing.T, cluster *clusterv1.Cluster, owners []metav1.OwnerReference) ca {
	t.Helper()
	certificate := &secret.Certificate{Purpose: secret.ClusterCA}
	if err := certificate.Generate(); err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{cluster.Name: {Server: "https://203.0.113.10:31443", CertificateAuthorityData: certificate.KeyPair.Cert}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"admin": {Token: "admin-token"}},
		Contexts:       map[string]*clientcmdapi.Context{cluster.Name: {Cluster: cluster.Name, AuthInfo: "admin"}},
		CurrentContext: cluster.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]map[string][]byte{
		lodiav1alpha1.CABundleSecret.Name(cluster.Name):        {lodiav1alpha1.CACertKey: certificate.KeyPair.Cert},
		lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name): {lodiav1alpha1.KubeconfigKey: kubeconfig},
		cluster.Name + "-cakey-cluster":                        {lodiav1alpha1.TLSCertKey: certificate.KeyPair.Cert, lodiav1alpha1.TLSPrivateKeyKey: certificate.KeyPair.Key},
	} {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       cluster.Namespace,
				Name:            name,
				Labels:          map[string]string{lodiav1alpha1.ControlPlaneLabel: cluster.Name},
				OwnerReferences: owners,
			},
			Data: data,
		}
		if err := k8s.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	return ca{cert: certificate.KeyPair.Cert, key: certificate.KeyPair.Key}
}

func setReady(t *testing.T, cp *lodiav1alpha1.ControlPlane) {
	t.Helper()
	now := metav1.Now()
	cp.Status = lodiav1alpha1.ControlPlaneStatus{
		ObservedGeneration: cp.Generation,
		Endpoint: &lodiav1alpha1.EndpointStatus{
			APIServer:    lodiav1alpha1.HostPort{Host: "203.0.113.10", Port: 31443},
			Konnectivity: lodiav1alpha1.HostPort{Host: "203.0.113.10", Port: 31132},
			InCluster:    lodiav1alpha1.HostPort{Host: "203.0.113.10", Port: 31643},
		},
		Kubernetes: &lodiav1alpha1.KubernetesStatus{Version: "v1.35.8"},
		Conditions: []metav1.Condition{
			{
				Type: lodiav1alpha1.ConditionReachable, Status: metav1.ConditionTrue, Reason: lodiav1alpha1.ReasonReachable,
				Message: "the public endpoint answers /readyz", ObservedGeneration: cp.Generation, LastTransitionTime: now,
			},
			{
				Type: lodiav1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: lodiav1alpha1.ReasonReady,
				Message: "every readiness condition is True", ObservedGeneration: cp.Generation, LastTransitionTime: now,
			},
		},
	}
	if err := k8s.Status().Update(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
}

func controlPlaneOf(cluster *clusterv1.Cluster) *lodiav1alpha1.ControlPlane {
	return &lodiav1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: cluster.Name}}
}

func secretOf(cluster *clusterv1.Cluster, purpose secret.Purpose) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: secret.Name(cluster.Name, purpose)}}
}

func absent(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, obj := range objs {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s: want it absent, got %v", obj, obj.GetName(), err)
		}
	}
}

func ready(lcp *v1alpha1.LodiaControlPlane, status metav1.ConditionStatus, reason, message string) error {
	c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition)
	if c == nil || c.Status != status || c.Reason != reason || !strings.Contains(c.Message, message) {
		return fmt.Errorf("Ready is %+v, want %s %s with %q", c, status, reason, message)
	}
	return nil
}

func eventually(t *testing.T, check func() error) {
	t.Helper()
	var last error
	err := wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
		last = check()
		return last == nil, nil
	})
	if err != nil {
		t.Fatalf("%v: %v", err, last)
	}
}

func consistently(t *testing.T, d time.Duration, check func() error) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if err := check(); err != nil {
			t.Fatal(err)
		}
	}
}

func isSecretApply(obj runtime.ApplyConfiguration, name string) bool {
	s, ok := obj.(*corev1ac.SecretApplyConfiguration)
	return ok && s.Name != nil && *s.Name == name
}

func condition(lcp *v1alpha1.LodiaControlPlane, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(lcp.Status.Conditions, conditionType)
}

func available(t *testing.T, lcp *v1alpha1.LodiaControlPlane, status metav1.ConditionStatus, reason string) {
	t.Helper()
	if c := condition(lcp, clusterv1.AvailableCondition); c == nil || c.Status != status || c.Reason != reason {
		t.Errorf("Available is %+v, want %s %s", c, status, reason)
	}
}

func initialized(t *testing.T, lcp *v1alpha1.LodiaControlPlane) {
	t.Helper()
	if i := lcp.Status.Initialization.ControlPlaneInitialized; i == nil || !*i {
		t.Errorf("not initialized: %+v", lcp.Status.Initialization)
	}
}

func updateSpec(t *testing.T, lcp *v1alpha1.LodiaControlPlane, change func(*v1alpha1.LodiaControlPlane)) {
	t.Helper()
	before := lcp.DeepCopy()
	change(lcp)
	if err := k8s.Patch(t.Context(), lcp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
}

func createdControlPlane(t *testing.T, name string) (*clusterv1.Cluster, *v1alpha1.LodiaControlPlane, *lodiav1alpha1.ControlPlane, *controller.Reconciler) {
	t.Helper()
	cluster, lcp := create(t, namespace(t), name)
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	return cluster, lcp, cp, r
}

func readyControlPlane(t *testing.T, name string) (*clusterv1.Cluster, *v1alpha1.LodiaControlPlane, *lodiav1alpha1.ControlPlane, *controller.Reconciler, ca) {
	t.Helper()
	cluster, lcp, cp, r := createdControlPlane(t, name)
	setReady(t, cp)
	issued := lodiaSecrets(t, cluster, ownedBy(cp))
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)
	return cluster, lcp, cp, r, issued
}
