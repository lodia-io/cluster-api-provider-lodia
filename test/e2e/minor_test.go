// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e && minor

package e2e

import (
	"bytes"
	"context"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const minorFrom = "v1.36.4"

func TestAMinorStepHoldsNewWorkersUntilLodiaRunsIt(t *testing.T) {
	var (
		k8s    client.Client
		key    client.ObjectKey
		tenant *tenantAccess
		first  *clusterv1.Machine
		step   *minorStep
		second *clusterv1.Machine
		owned  []client.Object
	)
	f := features.New("a minor step of the control plane holds the new workers until Lodia has run it, and leaves the worker that runs alone").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			name, err := clusterName(cfg)
			if err != nil {
				t.Fatal(err)
			}
			key = client.ObjectKey{Namespace: namespace, Name: name}
			if k8s, err = newClient(cfg); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the objects are applied at "+minorFrom+", and the DevCluster, whose infrastructure the suite manages, is marked provisioned", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			bounded, cancel := endAfter(ctx, minorBounds.Apply)
			defer cancel()
			if err := applyTheCluster(bounded, cfg, k8s, key, minorFrom); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("the Cluster reports ControlPlaneInitialized", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, minorBounds.Initialized, "ControlPlaneInitialized is True", func(ctx context.Context) error {
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
			until(ctx, t, minorBounds.Kindnet, "kindnet is applied", func(ctx context.Context) error {
				return decoder.DecodeEach(ctx, bytes.NewReader(rendered), ignoreExisting(decoder.CreateHandler(tenant.resources)))
			})
			return ctx
		}).
		Assess("the Machine of md-0 is Running with a nodeRef", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, minorBounds.Running, "the Machine of md-0 is Running with a nodeRef", func(ctx context.Context) error {
				workers, err := runningWorkersOf(ctx, k8s, key, workersName(key.Name), 1)
				if err != nil {
					return err
				}
				first = &workers[0]
				return nil
			})
			return ctx
		}).
		Assess("its Node carries the DevMachine's providerID, has no uninitialized taint, is Ready and runs kubelet "+minorFrom, func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, minorBounds.Ready, "Node "+first.Status.NodeRef.Name+" has the providerID, no uninitialized taint, is Ready and runs kubelet "+minorFrom, func(ctx context.Context) error {
				joined, err := requireJoined(ctx, k8s, tenant, first)
				if err != nil {
					return err
				}
				return requireKubelet(joined, minorFrom)
			})
			return ctx
		}).
		Assess("the Cluster is Available", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, minorBounds.Available, "Available is True", func(ctx context.Context) error {
				return requireAvailable(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("spec.version is raised to "+newestKubernetes+" and md-1 at that version is created, and no Machine of md-1 comes before the LodiaControlPlane reports it and Lodia has run every part", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := raiseTheVersion(ctx, k8s, key, newestKubernetes); err != nil {
				t.Fatal(err)
			}
			manifest, err := render("workers-next.yaml", testdataVariables(key.Name, newestKubernetes))
			if err != nil {
				t.Fatal(err)
			}
			if err := createAll(ctx, k8s, manifest); err != nil {
				t.Fatal(err)
			}
			step = newMinorStep(key, newestKubernetes)
			until(ctx, t, minorBounds.Stepped, "the LodiaControlPlane reports "+newestKubernetes+", and Lodia's ControlPlane has no upgrade and every part at it", func(ctx context.Context) error {
				return step.poll(ctx, k8s)
			})
			if err := step.verdict(); err != nil {
				t.Fatal(err)
			}
			t.Logf("the MachineSet of %s held its Machine: %s", step.deployment, step.held)
			t.Logf("the Ready of the LodiaControlPlane read %v", step.readyHistory)
			return ctx
		}).
		Assess("the Machine of md-1 is Running with a nodeRef", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, minorBounds.NextRunning, "the Machine of md-1 is Running with a nodeRef", func(ctx context.Context) error {
				workers, err := runningWorkersOf(ctx, k8s, key, step.deployment, 1)
				if err != nil {
					return err
				}
				second = &workers[0]
				return nil
			})
			return ctx
		}).
		Assess("its Node carries the DevMachine's providerID, has no uninitialized taint, is Ready and runs kubelet "+newestKubernetes, func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			until(ctx, t, minorBounds.NextReady, "Node "+second.Status.NodeRef.Name+" has the providerID, no uninitialized taint, is Ready and runs kubelet "+newestKubernetes, func(ctx context.Context) error {
				joined, err := requireJoined(ctx, k8s, tenant, second)
				if err != nil {
					return err
				}
				return requireKubelet(joined, newestKubernetes)
			})
			return ctx
		}).
		Assess("the Machine of md-0 is the one it was, its Node is Ready at kubelet "+minorFrom+" through the tenant opened again, and the Cluster and the LodiaControlPlane are Available", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			reopened, err := reopenTenant(ctx, k8s, key)
			if err != nil {
				t.Fatal(err)
			}
			tenant = reopened
			t.Logf("the tenant's API server %s accepts the credential of the Secret as the provider holds it now", tenant.cfg.Host)
			until(ctx, t, minorBounds.Kept, "the Machine of md-0 and its Node are as they were, and the Cluster and the LodiaControlPlane are Available", func(ctx context.Context) error {
				if err := requireTheMachineKept(ctx, k8s, key, workersName(key.Name), first); err != nil {
					return err
				}
				joined, err := requireJoined(ctx, k8s, tenant, first)
				if err != nil {
					return err
				}
				if err := requireKubelet(joined, minorFrom); err != nil {
					return err
				}
				if err := requireAvailable(ctx, k8s, key); err != nil {
					return err
				}
				return requireControlPlaneAvailable(ctx, k8s, key)
			})
			return ctx
		}).
		Assess("deleting the Cluster removes the LodiaControlPlane, the ControlPlane, the etcd claim, the copied Secrets and both workers' containers", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			var err error
			if owned, err = deleteTheCluster(ctx, k8s, key, 2); err != nil {
				t.Fatal(err)
			}
			until(ctx, t, minorBounds.Deleted, "everything the Cluster made is gone", func(ctx context.Context) error {
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
