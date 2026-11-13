// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Package clusterctlproxy gives clusterctl's library a Proxy of the suite's
// own, so that every call of its client reaches the cluster that a
// *rest.Config names, within the timeouts its caller chooses, and none reads a
// kubeconfig path or the current context.
//
// clusterctl's own proxy takes one timeout for requests and discovery alike,
// and loads KUBECONFIG and ~/.kube/config when no path is given:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/proxy.go#L392-L409
package clusterctlproxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1beta1 "sigs.k8s.io/cluster-api/api/core/v1beta1" //nolint:staticcheck // clusterctl's own scheme holds the v1beta1 kinds
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Kubeconfig is what to give clusterctl for the cluster of the Proxy:
// clusterctl's Move refuses an empty ToKubeconfig, and the factory refuses any
// other kubeconfig, since it would be ignored.
var Kubeconfig = clusterctl.Kubeconfig{Context: "the-cluster-of-the-proxy"}

// New returns clusterctl's client, with every cluster client it makes, both
// ends of a move included, built by a Proxy over cfg. Requests of the Proxy's
// clients time out at requestTimeout, and their discovery at discoveryTimeout.
func New(ctx context.Context, configClient config.Client, cfg *rest.Config, requestTimeout, discoveryTimeout time.Duration) (clusterctl.Client, error) {
	// clusterctl sets these so that it is not throttled when it lists every
	// kind of a large cluster:
	// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/proxy.go#L155-L159
	cfg = rest.CopyConfig(cfg)
	cfg.QPS = 500
	cfg.Burst = 1000
	p := &proxy{cfg: cfg, requestTimeout: requestTimeout, discoveryTimeout: discoveryTimeout}
	return clusterctl.New(ctx, "", clusterctl.InjectConfig(configClient), clusterctl.InjectClusterClientFactory(clusterClientFactory(configClient, p)))
}

func clusterClientFactory(configClient config.Client, p cluster.Proxy) clusterctl.ClusterClientFactory {
	return func(in clusterctl.ClusterClientFactoryInput) (cluster.Client, error) {
		if err := requireNoKubeconfig(in.Kubeconfig); err != nil {
			return nil, err
		}
		return cluster.New(cluster.Kubeconfig{}, configClient, cluster.InjectProxy(p), cluster.InjectYamlProcessor(in.Processor)), nil
	}
}

func requireNoKubeconfig(kubeconfig clusterctl.Kubeconfig) error {
	if kubeconfig != (clusterctl.Kubeconfig{}) && kubeconfig != Kubeconfig {
		return fmt.Errorf("clusterctl was given the kubeconfig %+v, which the suite's proxy would ignore: it reaches the one cluster of its rest config, and wants no kubeconfig or clusterctlproxy.Kubeconfig", kubeconfig)
	}
	return nil
}

// NewClient returns a client of the cluster that cfg names. Its requests time
// out at requestTimeout and its discovery, through a REST mapper of its own, at
// discoveryTimeout: controller-runtime's mapper discovers without the request's
// context, so it ends only at the timeout of the config it is built on.
// https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.2/pkg/client/apiutil/restmapper.go#L285
func NewClient(cfg *rest.Config, scheme *runtime.Scheme, requestTimeout, discoveryTimeout time.Duration) (client.Client, error) {
	requests := rest.CopyConfig(cfg)
	requests.Timeout = requestTimeout
	discovery := rest.CopyConfig(cfg)
	discovery.Timeout = discoveryTimeout
	httpClient, err := rest.HTTPClientFor(discovery)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(discovery, httpClient)
	if err != nil {
		return nil, err
	}
	return client.New(requests, client.Options{Scheme: scheme, Mapper: mapper})
}

type proxy struct {
	cfg                              *rest.Config
	requestTimeout, discoveryTimeout time.Duration
}

var _ cluster.Proxy = &proxy{}

// The kinds of clusterctl's own scheme, which is in an internal package:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/internal/scheme/scheme.go#L41-L50
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, clusterctlv1.AddToScheme, clusterv1beta1.AddToScheme, clusterv1.AddToScheme,
		apiextensionsv1.AddToScheme, apiextensionsv1beta1.AddToScheme, admissionregistrationv1.AddToScheme,
		admissionregistrationv1beta1.AddToScheme, controlplanev1.AddToScheme, addonsv1.AddToScheme,
	} {
		utilruntime.Must(add(s))
	}
	return s
}()

func (p *proxy) GetConfig() (*rest.Config, error) {
	cfg := rest.CopyConfig(p.cfg)
	cfg.Timeout = p.requestTimeout
	return cfg, nil
}

func (p *proxy) ValidateKubernetesVersion() error {
	cfg := rest.CopyConfig(p.cfg)
	cfg.Timeout = p.discoveryTimeout
	return version.CheckKubernetesVersion(cfg, version.MinimumKubernetesVersion)
}

func (p *proxy) NewClient(context.Context) (client.Client, error) {
	return NewClient(p.cfg, scheme, p.requestTimeout, p.discoveryTimeout)
}

func (p *proxy) CurrentNamespace() (string, error) {
	return "", errors.New("the proxy has no kubeconfig and so no current namespace: name the namespace")
}

func (p *proxy) CheckClusterAvailable(context.Context) error {
	return errors.New("CheckClusterAvailable is not on the path of Init or Move")
}

func (p *proxy) ListResources(context.Context, map[string]string, ...string) ([]unstructured.Unstructured, error) {
	return nil, errors.New("ListResources is not on the path of Init or Move")
}

func (p *proxy) GetContexts(string) ([]string, error) {
	return nil, errors.New("the proxy has no kubeconfig and so no contexts")
}

func (p *proxy) GetResourceNames(context.Context, string, string, []client.ListOption, string) ([]string, error) {
	return nil, errors.New("GetResourceNames is not on the path of Init or Move")
}
