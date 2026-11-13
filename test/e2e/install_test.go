// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	logf "sigs.k8s.io/cluster-api/cmd/clusterctl/log"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

const lodiaRelease = "lodia"

type release struct {
	url    string
	sha256 string
	images map[string]string
}

var (
	certManager = release{
		url:    "https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml",
		sha256: "c04a51295adda2b1a775146751f973e787190778c7f35180861d9678876e5c74",
		images: map[string]string{
			"quay.io/jetstack/cert-manager-cainjector:v1.21.3": "sha256:083d0e906f8eb44e255fe4ff817ae54e49f9ed82e22f1011bb4e9e4f4b8a641f",
			"quay.io/jetstack/cert-manager-controller:v1.21.3": "sha256:9f4f96a33c7774cef24466fcd85031997531a03a8d117f151126baa8d1290dcc",
			"quay.io/jetstack/cert-manager-webhook:v1.21.3":    "sha256:055ac8e9de6a0ba6763794838fe38944acc650f68ebe1b01dd8be079e580f458",
		},
	}
	clusterAPI = release{
		url:    "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.15.0-beta.0/core-components.yaml",
		sha256: "5d2768373aad013796033c583e14ca873421a21c368bd48c9df0ef60be2c70cc",
		images: map[string]string{
			"registry.k8s.io/cluster-api/cluster-api-controller:v1.15.0-beta.0": "sha256:54881292f942ef6f225be8966f1b064ed51bb109c84eadd2477f2976f1f0b62e",
		},
	}
	kubeadmBootstrap = release{
		url:    "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.15.0-beta.0/bootstrap-components.yaml",
		sha256: "51ecc34dacad861b46507d252fe583aa0b693d2feb333216652aa9ceb8331069",
		images: map[string]string{
			"registry.k8s.io/cluster-api/kubeadm-bootstrap-controller:v1.15.0-beta.0": "sha256:06e8bd16a0d477a825ac94f16f511a24e316000e8ae93a2f9defb7e5e58d58b7",
		},
	}
	clusterAPIMetadata = release{
		url:    "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.15.0-beta.0/metadata.yaml",
		sha256: "82a36f78f0e16b547b23de41e2bceb0e8349de5188ec9eb5f7780b5d8cb5177d",
	}
	clusterAPIDocker = release{
		url:    "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.15.0-beta.0/infrastructure-components-development.yaml",
		sha256: "6d6a6076ba738daf287f7e1bc6d80543bf82d2761bb7364eb92ffaae223e12a5",
		images: map[string]string{
			"gcr.io/k8s-staging-cluster-api/capd-manager:v1.15.0-beta.0": "sha256:b51569e3f9a91f26941c10364b461cdb03be3be4895745f24e522d1b2b547004",
		},
	}
)

func (r release) fetch(ctx context.Context, dir string) ([]byte, error) {
	cached := filepath.Join(dir, path.Base(r.url))
	raw, err := os.ReadFile(cached)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if checksum(raw) != r.sha256 {
		if raw, err = download(ctx, r.url); err != nil {
			return nil, err
		}
		if got := checksum(raw); got != r.sha256 {
			return nil, fmt.Errorf("%s has sha256 %s, the suite pins %s", r.url, got, r.sha256)
		}
		if err := os.WriteFile(cached, raw, 0o644); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func (r release) version() string {
	return path.Base(path.Dir(r.url))
}

func (r release) manifest(ctx context.Context, dir string) ([]byte, error) {
	raw, err := r.fetch(ctx, dir)
	if err != nil {
		return nil, err
	}
	rendered, err := yamlprocessor.NewSimpleProcessor().Process(raw, func(name string) (string, error) {
		return "", fmt.Errorf("%s is not set", name)
	})
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", r.url, err)
	}
	pinned, err := requireDigests(rendered, r.images)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.url, err)
	}
	return pinned, nil
}

func (r release) unrendered(ctx context.Context, dir string) ([]byte, error) {
	raw, err := r.fetch(ctx, dir)
	if err != nil {
		return nil, err
	}
	pinned, err := requireDigests(raw, r.images)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.url, err)
	}
	return pinned, nil
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answers %s", url, resp.Status)
	}
	return body, nil
}

func createWhenWebhooksAnswer(c client.Client) decoder.HandlerFunc {
	return func(ctx context.Context, obj k8s.Object) error {
		var last error
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			last = c.Create(ctx, obj)
			if last != nil && webhookNotReady(last) {
				return false, nil
			}
			return true, last
		})
		if wait.Interrupted(err) {
			return last
		}
		return err
	}
}

func ignoreExisting(create decoder.HandlerFunc) decoder.HandlerFunc {
	return func(ctx context.Context, obj k8s.Object) error {
		return client.IgnoreAlreadyExists(create(ctx, obj))
	}
}

func webhookNotReady(err error) bool {
	msg := err.Error()
	return apierrors.IsInternalError(err) && (strings.Contains(msg, "webhook") || strings.Contains(msg, "x509"))
}

func apply(ctx context.Context, cfg *envconf.Config, manifest []byte) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	return decoder.DecodeEach(ctx, bytes.NewReader(manifest), createWhenWebhooksAnswer(c))
}

func applyRelease(ctx context.Context, cfg *envconf.Config, r release) error {
	manifest, err := r.manifest(ctx, workDir(cfg))
	if err != nil {
		return err
	}
	if err := apply(ctx, cfg, manifest); err != nil {
		return fmt.Errorf("apply %s: %w", r.url, err)
	}
	return nil
}

func available(ctx context.Context, cfg *envconf.Config, namespace string, names ...string) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	for _, name := range names {
		isAvailable := deploymentAvailable(c, client.ObjectKey{Namespace: namespace, Name: name})
		if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, isAvailable); err != nil {
			return fmt.Errorf("deployment %s/%s is not Available: %w", namespace, name, err)
		}
	}
	return nil
}

func installCertManager(ctx context.Context, cfg *envconf.Config) error {
	if err := applyRelease(ctx, cfg, certManager); err != nil {
		return err
	}
	return available(ctx, cfg, "cert-manager", "cert-manager", "cert-manager-cainjector", "cert-manager-webhook")
}

func installLodia(ctx context.Context, cfg *envconf.Config) error {
	lodia := filepath.Join(root, "work", "lodia")
	chart := filepath.Join(lodia, "charts", "lodia")
	version, err := lodiaVersion(ctx, lodia)
	if err != nil {
		return err
	}
	args := []string{"--install", "--create-namespace"}
	for _, build := range []struct{ values, command string }{{"image", "./cmd/lodia"}, {"signerImage", "./cmd/lodia-signer"}, {"tunnelImage", "./cmd/lodia-tunnel"}} {
		out, err := ko(ctx, cfg, lodia, []string{"VERSION=" + version}, build.command)
		if err != nil {
			return err
		}
		named, err := reference.ParseNamed(strings.TrimSpace(string(out)))
		if err != nil {
			return err
		}
		image, ok := named.(reference.NamedTagged)
		if !ok {
			return fmt.Errorf("ko named %s without a tag", named)
		}
		args = append(args, "--set", build.values+".repository="+image.Name(), "--set", build.values+".tag="+image.Tag())
	}
	if out, err := exec.CommandContext(ctx, "helm", "dependency", "build", chart, "--kubeconfig", cfg.KubeconfigFile()).CombinedOutput(); err != nil {
		return fmt.Errorf("build the chart's dependencies: %w\n%s", err, out)
	}
	err = helm.New(cfg.KubeconfigFile()).RunUpgrade(
		helm.WithName(lodiaRelease), helm.WithChart(chart), helm.WithNamespace(lodiaNamespace), helm.WithContext(ctx),
		helm.WithArgs(args...), helm.WithWait(), helm.WithTimeout("5m"))
	if err != nil {
		return fmt.Errorf("install the chart: %w", err)
	}
	return nil
}

const (
	kubeadmBootstrapNamespace = "capi-kubeadm-bootstrap-system"
	kubeadmBootstrapAccount   = "capi-kubeadm-bootstrap-manager"
)

func installProviders(ctx context.Context, cfg *envconf.Config) error {
	dir := workDir(cfg)
	metadata, err := clusterAPIMetadata.fetch(ctx, dir)
	if err != nil {
		return err
	}
	var providers []clusterctlProvider
	for _, p := range []struct {
		name string
		kind clusterctlv1.ProviderType
		from release
	}{
		{"cluster-api", clusterctlv1.CoreProviderType, clusterAPI},
		{"kubeadm", clusterctlv1.BootstrapProviderType, kubeadmBootstrap},
		{"docker", clusterctlv1.InfrastructureProviderType, clusterAPIDocker},
	} {
		components, err := p.from.unrendered(ctx, dir)
		if err != nil {
			return err
		}
		providers = append(providers, clusterctlProvider{p.name, p.kind, p.from.version(), components, metadata})
	}
	lodia, err := lodiaProvider(ctx, cfg)
	if err != nil {
		return err
	}
	providers = append(providers, lodia)
	var components [][]byte
	for _, p := range providers {
		components = append(components, p.components)
	}
	if err := requireNoClusterctlSettingsInEnvironment(os.Environ(), components...); err != nil {
		return err
	}
	configPath, err := writeRepository(repositoryDir(cfg), providers, map[string]string{
		"LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE": kubeadmBootstrapNamespace,
		"LODIA_KUBEADM_BOOTSTRAP_SA_NAME":      kubeadmBootstrapAccount,
	})
	if err != nil {
		return err
	}
	logf.SetLogger(funcr.New(func(prefix, args string) { fmt.Println("clusterctl:", prefix, args) }, funcr.Options{Verbosity: 1}))
	c, err := newClusterctl(ctx, cfg, configPath)
	if err != nil {
		return err
	}
	if _, err := c.Init(ctx, clusterctl.InitOptions{
		CoreProvider:            "cluster-api",
		BootstrapProviders:      []string{"kubeadm"},
		InfrastructureProviders: []string{"docker"},
		ControlPlaneProviders:   []string{"lodia"},
		WaitProviders:           true,
		WaitProviderTimeout:     providerWait,
	}); err != nil {
		return fmt.Errorf("clusterctl init: %w", err)
	}
	return requireTheKubeadmBootstrapGrant(ctx, cfg)
}

func repositoryDir(cfg *envconf.Config) string {
	return filepath.Join(workDir(cfg), "repository")
}

func lodiaProvider(ctx context.Context, cfg *envconf.Config) (clusterctlProvider, error) {
	metadata, err := os.ReadFile(filepath.Join(root, "metadata.yaml"))
	if err != nil {
		return clusterctlProvider{}, err
	}
	version, err := developmentVersion(metadata)
	if err != nil {
		return clusterctlProvider{}, err
	}
	components, err := renderProvider(ctx, cfg)
	if err != nil {
		return clusterctlProvider{}, err
	}
	return clusterctlProvider{"lodia", clusterctlv1.ControlPlaneProviderType, version, components, metadata}, nil
}

func renderProvider(ctx context.Context, cfg *envconf.Config) ([]byte, error) {
	cluster, err := kindCluster(cfg.KubeconfigFile())
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(workDir(cfg), "release")
	if _, err := toolOutput(releaseComponentsCommand(ctx, cluster, dir)); err != nil {
		return nil, fmt.Errorf("make release-components into kind: %w", err)
	}
	return os.ReadFile(filepath.Join(dir, "control-plane-components.yaml"))
}

func requireTheKubeadmBootstrapGrant(ctx context.Context, cfg *envconf.Config) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: kubeadmBootstrapNamespace, Name: "capi-kubeadm-bootstrap-controller-manager"}
	if err := c.Get(ctx, key, deployment); err != nil {
		return err
	}
	return requireOnlyGet(ctx, c, deployment)
}

func lodiaVersion(ctx context.Context, lodia string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", lodia, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("name the Lodia build: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func ko(ctx context.Context, cfg *envconf.Config, dir string, env []string, args ...string) ([]byte, error) {
	cluster, err := kindCluster(cfg.KubeconfigFile())
	if err != nil {
		return nil, err
	}
	run := append([]string{"run", koModule, "build", "--base-import-paths", "--platform=linux/" + goruntime.GOARCH}, args...)
	cmd := exec.CommandContext(ctx, "go", run...)
	cmd.Dir = dir
	cmd.Env = koEnv(os.Environ(), cluster, env...)
	out, err := toolOutput(cmd)
	if err != nil {
		return nil, fmt.Errorf("ko build into kind: %w", err)
	}
	return out, nil
}
