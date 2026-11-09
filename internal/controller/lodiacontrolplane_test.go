// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.lodia.io/lodia/api/crds"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	"sigs.k8s.io/controller-runtime/pkg/cache"
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

func startManager(t *testing.T, mutate ...func(*cache.Options)) {
	t.Helper()
	cache, err := controller.CacheOptions()
	if err != nil {
		t.Fatal(err)
	}
	cache.ReaderFailOnMissingInformer = true
	for _, m := range mutate {
		m(&cache)
	}
	mgr, err := ctrl.NewManager(asController, ctrl.Options{
		Scheme:     controller.Scheme(),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Cache:      cache,
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&controller.Reconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
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

func create(t *testing.T, ns, name string, mutate ...func(*clusterv1.Cluster)) (*clusterv1.Cluster, *v1alpha1.LodiaControlPlane) {
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
	for _, m := range mutate {
		m(cluster)
	}
	if err := k8s.Create(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	lcp := &v1alpha1.LodiaControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name + "-cp",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: cluster.Name, UID: cluster.UID,
			}},
		},
		Spec: spec(),
	}
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

func TestAClusterGetsItsControlPlaneAndTheContract(t *testing.T) {
	startManager(t)
	cluster, lcp := create(t, namespace(t), "alpha")

	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	if !metav1.IsControlledBy(cp, lcp) || cp.Labels[clusterv1.ClusterNameLabel] != "alpha" {
		t.Errorf("ControlPlane owners %v, labels %v", cp.OwnerReferences, cp.Labels)
	}
	want := lodiav1alpha1.ControlPlaneSpec{
		Version:     "v1.35.8",
		Layout:      lodiav1alpha1.LayoutSplit,
		Datastore:   lcp.Spec.Datastore,
		Endpoint:    lcp.Spec.Endpoint,
		Placement:   lcp.Spec.Placement,
		Maintenance: lcp.Spec.Maintenance,
		Health:      lcp.Spec.Health,
		Network: lodiav1alpha1.Network{
			ServiceCIDRs:  []lodiav1alpha1.CIDR{"10.128.0.0/12"},
			PodCIDRs:      []lodiav1alpha1.CIDR{"192.168.0.0/16"},
			ClusterDomain: "tenant.local",
		},
	}
	if diff := cmp.Diff(want, cp.Spec); diff != "" {
		t.Errorf("ControlPlane spec (-want +got):\n%s", diff)
	}

	setReady(t, cp)
	issued := lodiaSecrets(t, cluster, ownedBy(cp))

	eventually(t, func() error {
		get(t, lcp)
		switch {
		case lcp.Spec.ControlPlaneEndpoint != clusterv1.APIEndpoint{Host: "203.0.113.10", Port: 31443}:
			return fmt.Errorf("endpoint %+v", lcp.Spec.ControlPlaneEndpoint)
		case lcp.Status.Initialization.ControlPlaneInitialized == nil || !*lcp.Status.Initialization.ControlPlaneInitialized:
			return errors.New("not initialized")
		case !meta.IsStatusConditionTrue(lcp.Status.Conditions, clusterv1.AvailableCondition):
			return fmt.Errorf("conditions %+v", lcp.Status.Conditions)
		case lcp.Status.Version != "v1.35.8" || len(lcp.Status.Versions) != 1 || lcp.Status.Versions[0].Version != "v1.35.8":
			return fmt.Errorf("version %q, versions %+v", lcp.Status.Version, lcp.Status.Versions)
		}
		return ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, "")
	})
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.AvailableCondition); c.Reason != lodiav1alpha1.ReasonReachable {
		t.Errorf("Available %+v, want Reachable's reason", c)
	}
	if lcp.Status.ExternalManagedControlPlane == nil || !*lcp.Status.ExternalManagedControlPlane {
		t.Error("externalManagedControlPlane is not true")
	}
	if _, ok := lcp.Annotations[clusterctlv1.BlockMoveAnnotation]; !ok {
		t.Errorf("annotations %v lack %s", lcp.Annotations, clusterctlv1.BlockMoveAnnotation)
	}

	admin := &corev1.Secret{}
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}, admin); err != nil {
		t.Fatal(err)
	}
	for purpose, data := range map[secret.Purpose]map[string][]byte{
		secret.Kubeconfig: {secret.KubeconfigDataName: admin.Data[lodiav1alpha1.KubeconfigKey]},
		secret.ClusterCA:  {secret.TLSCrtDataName: issued.cert},
	} {
		s := secretOf(cluster, purpose)
		eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(s), s) })
		if s.Type != clusterv1.ClusterSecretType || s.Labels[clusterv1.ClusterNameLabel] != "alpha" || !metav1.IsControlledBy(s, lcp) {
			t.Errorf("%s: type %s, labels %v, owners %v", s.Name, s.Type, s.Labels, s.OwnerReferences)
		}
		if diff := cmp.Diff(data, s.Data); diff != "" {
			t.Errorf("%s data (-want +got):\n%s", s.Name, diff)
		}
	}

	written := &corev1.SecretList{}
	if err := k8s.List(t.Context(), written, client.InNamespace(cluster.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, s := range written.Items {
		if !metav1.IsControlledBy(&s, lcp) {
			continue
		}
		for key, value := range s.Data {
			if key == secret.TLSKeyDataName || bytes.Contains(value, []byte("PRIVATE KEY")) || bytes.Contains(value, issued.key) {
				t.Errorf("Secret %s holds a private key under %s", s.Name, key)
			}
		}
	}
}

func TestANameLodiaRefusesIsReportedAndNothingIsCreated(t *testing.T) {
	startManager(t)
	name := strings.Repeat("a", 33)
	cluster, lcp := create(t, namespace(t), name)
	lodiaSecrets(t, cluster, nil)

	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.RefusedReason,
			fmt.Sprintf(`ControlPlane.lodia.io %q is invalid: <nil>: Invalid value: the name must be a DNS-1035 label of at most 32 characters`, name))
	})
	absent(t, controlPlaneOf(cluster), secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
}

func TestDeletingWaitsUntilTheControlPlaneIsGone(t *testing.T) {
	startManager(t)
	cluster, lcp := create(t, namespace(t), "gamma")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	before := cp.DeepCopy()
	cp.Finalizers = append(cp.Finalizers, lodiav1alpha1.DatastoreFinalizer)
	if err := k8s.Patch(t.Context(), cp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}

	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		get(t, cp)
		if cp.DeletionTimestamp.IsZero() {
			return errors.New("the ControlPlane is not being deleted")
		}
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, clusterv1.DeletingReason, "waiting for ControlPlane gamma to be deleted")
	})
	if !strings.Contains(strings.Join(lcp.Finalizers, ","), v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}

	before = cp.DeepCopy()
	cp.Finalizers = nil
	if err := k8s.Patch(t.Context(), cp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(lcp), lcp); err == nil {
			return fmt.Errorf("the LodiaControlPlane is still there with finalizers %v", lcp.Finalizers)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
	absent(t, cp)
}

func deleteTheCluster(t *testing.T, cluster *clusterv1.Cluster) {
	t.Helper()
	if err := k8s.Delete(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster); !apierrors.IsNotFound(err) {
			return fmt.Errorf("the Cluster: %v", err)
		}
		return nil
	})
}

func TestDeletionCompletesOnceTheClusterIsGone(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "phi")
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)
	absent(t, lcp)
	get(t, cp)
	if !cp.DeletionTimestamp.IsZero() {
		t.Error("the provider deleted the ControlPlane the garbage collector owns")
	}
}

func TestDeletionCompletesOnceTheOwnerReferenceIsGone(t *testing.T) {
	_, lcp, _, r, _ := readyControlPlane(t, "phi-ref")
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.OwnerReferences = nil })
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)
	absent(t, lcp)
}

func TestAFailedFinalizerRemovalIsRetried(t *testing.T) {
	cluster, lcp, _, r, _ := readyControlPlane(t, "phi-failed")
	deleteTheCluster(t, cluster)
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("etcdserver: leader changed")
	failing := direct(t, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return failure
		},
	})

	if err := pass(t.Context(), failing, lcp); !errors.Is(err, failure) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !strings.Contains(strings.Join(lcp.Finalizers, ","), v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
	passes(t, r, lcp)
	absent(t, lcp)
}

func TestATransientClusterReadWhileDeletingKeepsTheFinalizer(t *testing.T) {
	_, lcp, _, _, _ := readyControlPlane(t, "phi-read")
	if err := k8s.Delete(t.Context(), lcp); err != nil {
		t.Fatal(err)
	}
	unavailable := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*clusterv1.Cluster); ok {
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !strings.Contains(strings.Join(lcp.Finalizers, ","), v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
}

func TestAGoneClusterKeepsTheFinalizerUntilDeletion(t *testing.T) {
	cluster, lcp, _, r, _ := readyControlPlane(t, "phi-kept")
	deleteTheCluster(t, cluster)

	if err := pass(t.Context(), r, lcp); !apierrors.IsNotFound(err) {
		t.Fatalf("the pass returned %v", err)
	}
	get(t, lcp)
	if !strings.Contains(strings.Join(lcp.Finalizers, ","), v1alpha1.Finalizer) {
		t.Errorf("finalizers %v", lcp.Finalizers)
	}
}

func TestPauseStopsTheProvider(t *testing.T) {
	startManager(t)
	cluster, lcp := create(t, namespace(t), "delta", func(c *clusterv1.Cluster) { c.Spec.Paused = new(true) })
	paused := func(message string) func() error {
		return func() error {
			get(t, lcp)
			c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.PausedCondition)
			if c == nil || c.Status != metav1.ConditionTrue || c.Message != message {
				return fmt.Errorf("Paused is %+v", c)
			}
			return nil
		}
	}
	eventually(t, paused("Cluster spec.paused is set to true"))
	absent(t, controlPlaneOf(cluster))

	before := lcp.DeepCopy()
	metav1.SetMetaDataAnnotation(&lcp.ObjectMeta, clusterv1.PausedAnnotation, "")
	if err := k8s.Patch(t.Context(), lcp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	unpaused := cluster.DeepCopy()
	unpaused.Spec.Paused = new(false)
	if err := k8s.Patch(t.Context(), unpaused, client.MergeFrom(cluster)); err != nil {
		t.Fatal(err)
	}
	eventually(t, paused("LodiaControlPlane has the cluster.x-k8s.io/paused annotation"))
	absent(t, controlPlaneOf(cluster))

	before = lcp.DeepCopy()
	delete(lcp.Annotations, clusterv1.PausedAnnotation)
	if err := k8s.Patch(t.Context(), lcp, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		return k8s.Get(t.Context(), client.ObjectKeyFromObject(cluster), controlPlaneOf(cluster))
	})
}

func TestALeftoverControlPlaneOfAnotherOwnerIsLeftAlone(t *testing.T) {
	startManager(t)
	ns := namespace(t)
	previous := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "previous"}}
	if err := k8s.Create(t.Context(), previous); err != nil {
		t.Fatal(err)
	}
	leftover := &lodiav1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            "epsilon",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(previous, corev1.SchemeGroupVersion.WithKind("ConfigMap"))},
		},
		Spec: lodiav1alpha1.ControlPlaneSpec{
			Version:   "v1.35.0",
			Datastore: lodiav1alpha1.DatastoreChoice{Shared: &lodiav1alpha1.SharedDatastore{Name: "shared"}},
			Endpoint:  lodiav1alpha1.Endpoint{LoadBalancer: &lodiav1alpha1.LoadBalancerEndpoint{}},
			Network:   lodiav1alpha1.Network{ServiceCIDRs: []lodiav1alpha1.CIDR{"10.96.0.0/12"}, PodCIDRs: []lodiav1alpha1.CIDR{"10.244.0.0/16"}},
		},
	}
	if err := k8s.Create(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}
	cluster, lcp := create(t, ns, "epsilon")

	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.ObjectsControlledElsewhereReason,
			fmt.Sprintf("ControlPlane epsilon is controlled by ConfigMap previous (uid %s)", previous.UID))
	})
	cp := controlPlaneOf(cluster)
	get(t, cp)
	if cp.Spec.Version != "v1.35.0" || cp.Generation != leftover.Generation || !metav1.IsControlledBy(cp, previous) {
		t.Errorf("the leftover changed: version %s, generation %d, owners %v", cp.Spec.Version, cp.Generation, cp.OwnerReferences)
	}

	if err := k8s.Delete(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); err != nil {
			return err
		}
		if !metav1.IsControlledBy(cp, lcp) {
			return fmt.Errorf("owners %v", cp.OwnerReferences)
		}
		return nil
	})
}

func TestALeftoverSecretOutsideTheCacheIsLeftAlone(t *testing.T) {
	startManager(t)
	ns := namespace(t)
	leftover := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zeta-kubeconfig"},
		Data:       map[string][]byte{secret.KubeconfigDataName: []byte("a predecessor's kubeconfig")},
	}
	if err := k8s.Create(t.Context(), leftover); err != nil {
		t.Fatal(err)
	}
	cluster, lcp := create(t, ns, "zeta")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	lodiaSecrets(t, cluster, ownedBy(cp))

	eventually(t, func() error {
		get(t, lcp)
		return ready(lcp, metav1.ConditionFalse, v1alpha1.ObjectsControlledElsewhereReason, "Secret zeta-kubeconfig exists without a controller")
	})
	get(t, leftover)
	if string(leftover.Data[secret.KubeconfigDataName]) != "a predecessor's kubeconfig" || len(leftover.OwnerReferences) != 0 || len(leftover.Labels) != 0 {
		t.Errorf("the leftover changed: %v, owners %v, labels %v", leftover.Data, leftover.OwnerReferences, leftover.Labels)
	}
}

func isSecretApply(obj runtime.ApplyConfiguration, name string) bool {
	s, ok := obj.(*corev1ac.SecretApplyConfiguration)
	return ok && s.Name != nil && *s.Name == name
}

func TestAWriteFailingMidPassIsRetried(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "eta")
	failed := false
	r := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if isSecretApply(obj, "eta-ca") && !failed {
				failed = true
				return apierrors.NewInternalError(errors.New("etcdserver: leader changed"))
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))

	if err := pass(t.Context(), r, lcp); !apierrors.IsInternalError(err) {
		t.Fatalf("the pass returned %v, want the failed write's error", err)
	}
	get(t, secretOf(cluster, secret.Kubeconfig))
	absent(t, secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition); c != nil && c.Reason == v1alpha1.RefusedReason {
		t.Errorf("a server error reads as a refusal: %+v", c)
	}

	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
	get(t, secretOf(cluster, secret.ClusterCA))
}

func TestAFailureOnOneSecretDoesNotStopTheOther(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "chi")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))

	kubeconfigErr, caErr := errors.New("the kubeconfig write failed"), errors.New("the CA write failed")
	failing := map[string]error{"chi-kubeconfig": kubeconfigErr}
	broken := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if s, ok := obj.(*corev1ac.SecretApplyConfiguration); ok && failing[*s.Name] != nil {
				return failing[*s.Name]
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), broken, lcp); !errors.Is(err, kubeconfigErr) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))

	failing["chi-ca"] = caErr
	err := pass(t.Context(), broken, lcp)
	if !errors.Is(err, kubeconfigErr) || !errors.Is(err, caErr) {
		t.Fatalf("the pass returned %v, want both errors", err)
	}
}

func TestInitializationWaitsForBothSecrets(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "omega")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)
	notInitialized := func() {
		t.Helper()
		passes(t, r, lcp)
		get(t, lcp)
		if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
			t.Errorf("initialized without both Secrets: %+v", lcp.Status)
		}
		if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
			t.Error(err)
		}
	}
	notInitialized()

	issued := lodiaSecrets(t, cluster, ownedBy(cp))
	bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.CABundleSecret.Name(cluster.Name)}}
	if err := k8s.Delete(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	notInitialized()
	get(t, secretOf(cluster, secret.Kubeconfig))

	restored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: bundle.Name, Labels: map[string]string{lodiav1alpha1.ControlPlaneLabel: cluster.Name}, OwnerReferences: ownedBy(cp)},
		Data:       map[string][]byte{lodiav1alpha1.CACertKey: issued.cert},
	}
	if err := k8s.Create(t.Context(), restored); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)

	if err := k8s.Delete(t.Context(), restored); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)
}

func TestARestartBetweenWritesResumes(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "theta")
	untilBlockMove(t, direct(t, interceptor.Funcs{}), lcp)

	killed := errors.New("the process was killed")
	crashing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); ok {
				return killed
			}
			return c.Apply(ctx, obj, opts...)
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return killed
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return killed
		},
	})
	if err := pass(t.Context(), crashing, lcp); !errors.Is(err, killed) {
		t.Fatalf("the pass returned %v", err)
	}
	cp := controlPlaneOf(cluster)
	get(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))
	if err := pass(t.Context(), crashing, lcp); !errors.Is(err, killed) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if lcp.Status.Initialization.ControlPlaneInitialized != nil || meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition) != nil {
		t.Fatalf("the killed pass wrote the status: %+v", lcp.Status)
	}
	setReady(t, cp)

	restarted := direct(t, interceptor.Funcs{})
	if err := pass(t.Context(), restarted, lcp); err != nil {
		t.Fatal(err)
	}
	created := cp.UID
	get(t, cp)
	if cp.UID != created {
		t.Error("the restarted provider replaced the ControlPlane")
	}
	get(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if lcp.Status.Initialization.ControlPlaneInitialized == nil || !*lcp.Status.Initialization.ControlPlaneInitialized {
		t.Errorf("not initialized after the restart: %+v", lcp.Status)
	}
}

func TestATransientErrorIsRetriedWithoutAVerdict(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "iota")
	unavailable := true
	r := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "ControlPlane" && unavailable {
				unavailable = false
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	untilBlockMove(t, r, lcp)

	if err := pass(t.Context(), r, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, controlPlaneOf(cluster))
	get(t, lcp)
	if c := meta.FindStatusCondition(lcp.Status.Conditions, clusterv1.ReadyCondition); c != nil {
		t.Errorf("a transient error set Ready: %+v", c)
	}

	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
	get(t, controlPlaneOf(cluster))
}

func TestAnExpiredDeadlineStopsThePassAndTheNextOneFinishes(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "kappa")
	slow := true
	r := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); ok && slow {
				slow = false
				<-ctx.Done()
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	lodiaSecrets(t, cluster, ownedBy(cp))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := pass(ctx, r, lcp); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))

	if err := pass(t.Context(), r, lcp); err != nil {
		t.Fatal(err)
	}
	get(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))
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

func readyControlPlane(t *testing.T, name string) (*clusterv1.Cluster, *v1alpha1.LodiaControlPlane, *lodiav1alpha1.ControlPlane, *controller.Reconciler, ca) {
	t.Helper()
	cluster, lcp := create(t, namespace(t), name)
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)
	issued := lodiaSecrets(t, cluster, ownedBy(cp))
	passes(t, r, lcp)
	get(t, lcp)
	initialized(t, lcp)
	return cluster, lcp, cp, r, issued
}

func TestReadyIsMirroredOnlyAtTheCurrentGeneration(t *testing.T) {
	_, lcp, cp, r, _ := readyControlPlane(t, "lambda")
	if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
		t.Fatal(err)
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.Spec.Version = "v1.35.9" })
	passes(t, r, lcp)
	get(t, cp)
	if cp.Generation != 2 || cp.Spec.Version != "v1.35.9" {
		t.Fatalf("generation %d, version %s", cp.Generation, cp.Spec.Version)
	}
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionUnknown, lodiav1alpha1.ReasonLatestGenerationNotObserved, "generation 1 of 2"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReachable)
	initialized(t, lcp)

	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type: lodiav1alpha1.ConditionReachable, Status: metav1.ConditionFalse, Reason: lodiav1alpha1.ReasonExternalProbeFailed,
		Message: "the public endpoint does not answer", ObservedGeneration: 1,
	})
	if err := k8s.Status().Update(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	passes(t, r, lcp)
	get(t, lcp)
	available(t, lcp, metav1.ConditionFalse, lodiav1alpha1.ReasonExternalProbeFailed)

	setReady(t, cp)
	passes(t, r, lcp)
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionTrue, lodiav1alpha1.ReasonReady, ""); err != nil {
		t.Error(err)
	}
}

func TestARefusedChangeStillCopiesAndReports(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "mu")

	before := cluster.DeepCopy()
	cluster.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.200.0.0/16"}
	if err := k8s.Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	get(t, cp)
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type: lodiav1alpha1.ConditionReachable, Status: metav1.ConditionFalse, Reason: lodiav1alpha1.ReasonExternalProbeFailed,
		Message: "the public endpoint does not answer", ObservedGeneration: cp.Generation,
	})
	if err := k8s.Status().Update(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	renewed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}}
	get(t, renewed)
	renewed.Data[lodiav1alpha1.KubeconfigKey] = []byte("a renewed kubeconfig")
	if err := k8s.Update(t.Context(), renewed); err != nil {
		t.Fatal(err)
	}

	if err := pass(t.Context(), r, lcp); !apierrors.IsInvalid(err) {
		t.Fatalf("the pass returned %v, want the refusal", err)
	}
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.RefusedReason, "network is immutable"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, lodiav1alpha1.ReasonExternalProbeFailed)
	copied := secretOf(cluster, secret.Kubeconfig)
	get(t, copied)
	if string(copied.Data[secret.KubeconfigDataName]) != "a renewed kubeconfig" {
		t.Errorf("the kubeconfig did not follow the renewal: %s", copied.Data[secret.KubeconfigDataName])
	}
}

func TestAPredecessorsSecretsAreNotCopied(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "nu")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	predecessor := []metav1.OwnerReference{{
		APIVersion: lodiav1alpha1.GroupVersion.String(), Kind: "ControlPlane", Name: cluster.Name, UID: "the-predecessor", Controller: new(true),
	}}
	lodiaSecrets(t, cluster, predecessor)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	setReady(t, cp)

	passes(t, r, lcp)
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
	get(t, lcp)
	if i := lcp.Status.Initialization.ControlPlaneInitialized; i != nil {
		t.Errorf("initialized from a predecessor's Secrets: %+v", lcp.Status)
	}

	for _, name := range []string{
		lodiav1alpha1.CABundleSecret.Name(cluster.Name), lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name), cluster.Name + "-cakey-cluster",
	} {
		if err := k8s.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	lodiaSecrets(t, cluster, ownedBy(cp))
	passes(t, r, lcp)
	get(t, secretOf(cluster, secret.Kubeconfig))
	get(t, secretOf(cluster, secret.ClusterCA))
}

func TestASourceWithoutItsKeyKeepsTheLastGoodCopy(t *testing.T) {
	cluster, lcp, _, r, issued := readyControlPlane(t, "sigma")
	admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name)}}
	get(t, admin)
	kubeconfig := admin.Data[lodiav1alpha1.KubeconfigKey]
	admin.Data = map[string][]byte{"other": []byte("x")}
	if err := k8s.Update(t.Context(), admin); err != nil {
		t.Fatal(err)
	}
	bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: lodiav1alpha1.CABundleSecret.Name(cluster.Name)}}
	get(t, bundle)
	bundle.Data[lodiav1alpha1.CACertKey] = []byte{}
	if err := k8s.Update(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}

	passes(t, r, lcp)

	copied := secretOf(cluster, secret.Kubeconfig)
	get(t, copied)
	if !bytes.Equal(copied.Data[secret.KubeconfigDataName], kubeconfig) {
		t.Errorf("the good kubeconfig was replaced by %q", copied.Data[secret.KubeconfigDataName])
	}
	ca := secretOf(cluster, secret.ClusterCA)
	get(t, ca)
	if !bytes.Equal(ca.Data[secret.TLSCrtDataName], issued.cert) {
		t.Errorf("the good certificate was replaced by %q", ca.Data[secret.TLSCrtDataName])
	}
}

func TestASourceWithoutItsKeyWritesNoCopy(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "upsilon")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	passes(t, r, lcp)
	cp := controlPlaneOf(cluster)
	get(t, cp)
	for name, data := range map[string]map[string][]byte{
		lodiav1alpha1.AdminKubeconfigSecret.Name(cluster.Name): {"wrong": []byte("x")},
		lodiav1alpha1.CABundleSecret.Name(cluster.Name):        {lodiav1alpha1.CACertKey: {}},
	} {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name, Labels: map[string]string{lodiav1alpha1.ControlPlaneLabel: cluster.Name}, OwnerReferences: ownedBy(cp)},
			Data:       data,
		}
		if err := k8s.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}

	passes(t, r, lcp)
	absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
}

func TestAnInitializedControlPlaneIsNotRecreated(t *testing.T) {
	cluster, lcp, cp, r, issued := readyControlPlane(t, "xi")
	if err := k8s.Delete(t.Context(), cp); err != nil {
		t.Fatal(err)
	}

	stale := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if l, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				l.Status.Initialization.ControlPlaneInitialized = nil
			}
			return err
		},
	})
	stale.APIReader = r.APIReader
	passes(t, stale, lcp)
	passes(t, r, lcp)

	absent(t, cp)
	get(t, lcp)
	initialized(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason, "ControlPlane xi is gone"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason)
	kept := secretOf(cluster, secret.ClusterCA)
	get(t, kept)
	if !bytes.Equal(kept.Data[secret.TLSCrtDataName], issued.cert) {
		t.Error("the CA changed")
	}
}

func TestAControlPlaneVanishingBeforeTheApplyIsNotRecreated(t *testing.T) {
	cluster, lcp, cp, r, _ := readyControlPlane(t, "tau")
	racing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsConflict(err) {
		t.Fatalf("the pass returned %v, want a conflict on the UID", err)
	}
	absent(t, cp)

	passes(t, r, lcp)
	absent(t, cp)
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason, "ControlPlane tau is gone"); err != nil {
		t.Error(err)
	}
	available(t, lcp, metav1.ConditionFalse, v1alpha1.ControlPlaneNotFoundReason)
}

func TestAControlPlaneReplacedBeforeTheApplyIsLeftAlone(t *testing.T) {
	cluster, lcp, cp, _, _ := readyControlPlane(t, "tau-replaced")
	replacement := cp.DeepCopy()
	replacement.ObjectMeta = metav1.ObjectMeta{Namespace: cp.Namespace, Name: cp.Name, Finalizers: []string{lodiav1alpha1.DatastoreFinalizer}}
	racing := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok && replacement.UID == "" {
				if err := k8s.Delete(ctx, controlPlaneOf(cluster)); err != nil {
					return err
				}
				if err := k8s.Create(ctx, replacement); err != nil {
					return err
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if err := pass(t.Context(), racing, lcp); !apierrors.IsInvalid(err) {
		t.Fatalf("the pass returned %v, want the UID refused", err)
	}
	got := controlPlaneOf(cluster)
	get(t, got)
	if got.UID != replacement.UID || got.Generation != replacement.Generation || metav1.GetControllerOf(got) != nil {
		t.Errorf("the replacement changed: uid %s, generation %d, owners %v", got.UID, got.Generation, got.OwnerReferences)
	}
}

func TestAnExpiredDeadlineOnTheControlPlaneApplyIsReturned(t *testing.T) {
	_, lcp, cp, r, _ := readyControlPlane(t, "tau-slow")
	slow := direct(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if _, ok := obj.(*corev1ac.SecretApplyConfiguration); !ok {
				<-ctx.Done()
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := pass(ctx, slow, lcp); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the pass returned %v", err)
	}
	created := cp.UID
	get(t, cp)
	if cp.UID != created {
		t.Error("the ControlPlane was replaced")
	}
	passes(t, r, lcp)
}

func TestATransientErrorReadingInitializationCreatesNothing(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "omicron")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)

	unavailable := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	unavailable.Client = r.Client
	if err := pass(t.Context(), unavailable, lcp); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("the pass returned %v", err)
	}
	absent(t, controlPlaneOf(cluster))
}

func TestAResyncRepairsTheProvidersSecrets(t *testing.T) {
	options, err := controller.CacheOptions()
	if err != nil {
		t.Fatal(err)
	}
	if options.SyncPeriod == nil || *options.SyncPeriod != 10*time.Minute {
		t.Errorf("sync period %v", options.SyncPeriod)
	}

	startManager(t, func(o *cache.Options) { o.SyncPeriod = new(time.Second) })
	cluster, _ := create(t, namespace(t), "pi")
	cp := controlPlaneOf(cluster)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp) })
	issued := lodiaSecrets(t, cluster, ownedBy(cp))
	copied := secretOf(cluster, secret.ClusterCA)
	eventually(t, func() error { return k8s.Get(t.Context(), client.ObjectKeyFromObject(copied), copied) })

	copied.Data[secret.TLSCrtDataName] = []byte("edited by hand")
	if err := k8s.Update(t.Context(), copied); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(copied), copied); err != nil {
			return err
		}
		if !bytes.Equal(copied.Data[secret.TLSCrtDataName], issued.cert) {
			return fmt.Errorf("tls.crt is %s", copied.Data[secret.TLSCrtDataName])
		}
		return nil
	})
}

func TestTheRoleCoversLeaderElectionAndTheMetricsFilter(t *testing.T) {
	ns := namespace(t)
	mgr, err := ctrl.NewManager(asController, ctrl.Options{
		Scheme:                  controller.Scheme(),
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          true,
		LeaderElectionID:        "controlplane.lodia.io",
		LeaderElectionNamespace: ns,
	})
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
	eventually(t, func() error {
		lease := &coordinationv1.Lease{}
		if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "controlplane.lodia.io"}, lease); err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return errors.New("no holder")
		}
		events := &corev1.EventList{}
		if err := k8s.List(t.Context(), events, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Reason == "LeaderElection" {
				return nil
			}
		}
		return errors.New("no LeaderElection event")
	})

	for _, attributes := range []authorizationv1.ResourceAttributes{
		{Group: authenticationv1.GroupName, Resource: "tokenreviews", Verb: "create"},
		{Group: authorizationv1.GroupName, Resource: "subjectaccessreviews", Verb: "create"},
	} {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: asController.Impersonate.UserName, ResourceAttributes: &attributes,
		}}
		if err := k8s.Create(t.Context(), review); err != nil {
			t.Fatal(err)
		}
		if !review.Status.Allowed {
			t.Errorf("%s %s is not allowed: %s", attributes.Verb, attributes.Resource, review.Status.Reason)
		}
	}
}

func TestCABPKAcceptsTheCASecretWithoutAKey(t *testing.T) {
	cluster, _, _, _, issued := readyControlPlane(t, "rho")
	copied := secretOf(cluster, secret.ClusterCA)
	get(t, copied)
	if _, ok := copied.Data[secret.TLSKeyDataName]; ok {
		t.Fatalf("%s holds %s", copied.Name, secret.TLSKeyDataName)
	}

	certificates := secret.NewCertificatesForWorker("")
	if err := certificates.Lookup(t.Context(), k8s, client.ObjectKeyFromObject(cluster)); err != nil {
		t.Fatal(err)
	}
	if err := certificates.EnsureAllExist(); err != nil {
		t.Fatal(err)
	}
	ca := certificates.GetByPurpose(secret.ClusterCA)
	hashes, err := ca.Hashes()
	if err != nil || len(hashes) != 1 || !bytes.Equal(ca.KeyPair.Cert, issued.cert) {
		t.Errorf("hashes %v, %v; certificate %s", hashes, err, ca.KeyPair.Cert)
	}
}
