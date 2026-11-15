// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e && upgrade

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"go.lodia.io/cluster-api-provider-lodia/test/pin"
)

func upgradeProvider(ctx context.Context, cfg *envconf.Config, version string) error {
	c, err := newClusterctl(ctx, cfg, filepath.Join(repositoryDir(cfg), clusterctlConfigFile))
	if err != nil {
		return err
	}
	return c.ApplyUpgrade(ctx, clusterctl.ApplyUpgradeOptions{
		ControlPlaneProviders: []string{"lodia:" + version},
		WaitProviders:         true,
		WaitProviderTimeout:   providerWait,
	})
}

type upgrade struct {
	from, to     string
	was, becomes string
}

func theUpgrade(cfg *envconf.Config) (upgrade, error) {
	release, err := pin.Previous()
	if err != nil {
		return upgrade{}, err
	}
	metadata, err := os.ReadFile(filepath.Join(root, "metadata.yaml"))
	if err != nil {
		return upgrade{}, err
	}
	to, err := developmentVersion(metadata)
	if err != nil {
		return upgrade{}, err
	}
	u := upgrade{from: release.Version, to: to}
	for version, image := range map[string]*string{u.from: &u.was, u.to: &u.becomes} {
		components, err := os.ReadFile(filepath.Join(repositoryDir(cfg), clusterctlv1.ManifestLabel("lodia", clusterctlv1.ControlPlaneProviderType), version, "components.yaml"))
		if err != nil {
			return upgrade{}, err
		}
		if *image, err = managerImageIn(components); err != nil {
			return upgrade{}, fmt.Errorf("the components of %s: %w", version, err)
		}
	}
	return u, nil
}

func TestTheProviderUpgradesFromThePreviousRelease(t *testing.T) {
	var (
		k8s     client.Client
		key     client.ObjectKey
		tenant  *tenantAccess
		machine *clusterv1.Machine
		node    *corev1.Node
		before  *beforeTheUpgrade
		u       upgrade
		settled snapshot
	)
	f := features.New("clusterctl upgrades the provider from the previous release, and the new manager adopts what the release made").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			name, err := clusterName(cfg)
			if err != nil {
				t.Fatal(err)
			}
			key = client.ObjectKey{Namespace: namespace, Name: name}
			if k8s, err = newClient(cfg); err != nil {
				t.Fatal(err)
			}
			if u, err = theUpgrade(cfg); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the objects are applied under the release, and the DevCluster, whose infrastructure the suite manages, is marked provisioned", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			bounded, cancel := endAfter(ctx, upgradeBounds.Apply)
			defer cancel()
			if err := applyTheCluster(bounded, cfg, k8s, key); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the Cluster reports ControlPlaneInitialized", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, upgradeBounds.Initialized, "ControlPlaneInitialized is True", func(ctx context.Context) error {
				return requireInitialized(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("kindnet, from the management node's own manifest, is applied in the tenant", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			tenant = openTenant(ctx, t, k8s, key, tenantRequestTimeout)
			rendered, err := kindnetOf(ctx, cfg, k8s, key)
			if err != nil {
				t.Fatal(err)
			}
			until(ctx, t, upgradeBounds.Kindnet, "kindnet is applied", func(ctx context.Context) error {
				return decoder.DecodeEach(ctx, bytes.NewReader(rendered), ignoreExisting(decoder.CreateHandler(tenant.resources)))
			})
			return ctx
		}).
		Assess("the Machine is Running with a nodeRef", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, upgradeBounds.Running, "the Machine is Running with a nodeRef", func(ctx context.Context) error {
				workers, err := runningWorkers(ctx, k8s, key, 1)
				if err != nil {
					return err
				}
				machine = &workers[0]
				return nil
			})
			return ctx
		}).
		Assess("the Node carries the DevMachine's providerID, has no uninitialized taint and is Ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, upgradeBounds.Ready, "Node "+machine.Status.NodeRef.Name+" has the providerID, no uninitialized taint, and is Ready", func(ctx context.Context) error {
				joined, err := requireJoined(ctx, k8s, tenant, machine)
				if err != nil {
					return err
				}
				node = joined
				return nil
			})
			return ctx
		}).
		Assess("the Cluster is Available", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, upgradeBounds.Available, "Available is True", func(ctx context.Context) error {
				return requireAvailable(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("the release is what runs, and what it made is read: the UIDs, the Machine and the times of the Available conditions", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if _, err := requireProviderAt(ctx, k8s, u.from, u.was); err != nil {
				t.Fatal(err)
			}
			var err error
			if before, err = readBeforeTheUpgrade(ctx, k8s, key); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("clusterctl upgrades the provider to the tree's build", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			bounded, cancel := endAfter(ctx, upgradeBounds.Upgrade)
			defer cancel()
			if err := upgradeProvider(bounded, cfg, u.to); err != nil {
				t.Fatal(cutShort(bounded, fmt.Errorf("clusterctl upgrade apply --control-plane lodia:%s: %w", u.to, err)))
			}
			return ctx
		}).
		Assess("the inventory and a Deployment of a new UID say the tree's build and its image, and the kubeadm bootstrap provider's grant is get alone", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := requireTheProviderReplaced(ctx, k8s, before, u.to, u.becomes); err != nil {
				t.Fatal(err)
			}
			if err := requireTheKubeadmBootstrapGrant(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the binding of the grant is the one of before the upgrade", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := requireTheGrantKept(ctx, k8s, before); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the Lease of the manager is held by a pod of the new Deployment", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, upgradeBounds.Leader, "the Lease is held by a pod of the new Deployment", func(ctx context.Context) error {
				return requireTheNewManagerLeads(ctx, k8s)
			})
			return ctx
		}).
		Assess("with the Cluster paused, the new manager reports Paused True on the LodiaControlPlane, and once it is unpaused, False", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := whilePaused(ctx, k8s, key, upgradeBounds.Paused, 2*time.Second, nil); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the LodiaControlPlane, the ControlPlane and the copied Secrets are the objects of before, the copies are Lodia's, and no Available moved", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := requireWhatTheReleaseMadeKept(ctx, k8s, key, before); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the Machine is the one of before, the tenant accepts the credential as the provider holds it now, and the Node is Ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			kept, err := requireTheWorkerKept(ctx, k8s, key, before)
			if err != nil {
				t.Fatal(err)
			}
			if tenant, err = reopenTenant(ctx, k8s, key); err != nil {
				t.Fatal(err)
			}
			if node, err = requireJoined(ctx, k8s, tenant, kept); err != nil {
				t.Fatal(err)
			}
			t.Logf("Node %s is Ready through the tenant's API server %s", node.Name, tenant.cfg.Host)
			return ctx
		}).
		Assess("the resourceVersions of the namespace's objects stop changing", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			var err error
			if settled, err = settle(ctx, k8s, key.Namespace, upgradeBounds.Settled, settleInterval); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("and stay as they are for the whole window, but for what Lodia and etcd-druid write", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := hold(ctx, k8s, key.Namespace, upgradeBounds.Stable, holdInterval, settled); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("deleting the Cluster removes the LodiaControlPlane, the ControlPlane, the etcd claim, the copied Secrets and the worker's container", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			owned, err := deleteTheCluster(ctx, k8s, key)
			if err != nil {
				t.Fatal(err)
			}
			until(ctx, t, upgradeBounds.Deleted, "everything the Cluster made is gone", func(ctx context.Context) error {
				return requireGone(ctx, k8s, key, owned)
			})
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if t.Failed() {
				dump(ctx, cfg)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}
