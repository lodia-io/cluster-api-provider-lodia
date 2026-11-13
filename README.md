# cluster-api-provider-lodia

The [Cluster API](https://cluster-api.sigs.k8s.io/) control plane provider for [Lodia](https://github.com/lodia-io/lodia), which runs Kubernetes control planes as pods on a management cluster, one `ControlPlane` object per tenant cluster.

A `LodiaControlPlane` (`controlplane.lodia.io/v1alpha1`) owns exactly one Lodia `ControlPlane`, named after the `Cluster` and in its namespace.
The provider passes the `LodiaControlPlane`'s spec through to it, takes the network from `Cluster.spec.clusterNetwork`, and reports Lodia's status back in the form Cluster API's [control plane contract][contract] v1beta2 reads.
It creates no Machines and never writes to the `Cluster` or the InfraCluster.
Workers join through Cluster API's kubeadm bootstrap provider: [the e2e suite](test/e2e/README.md) joins a Docker machine to a cluster of this provider, and runs a pod on it.
`LodiaControlPlaneTemplate` holds the same spec, but the version, `controlPlaneEndpoint` and `restoreFrom`, for ClusterClasses.

How each rule of the contract is met, the life of the objects and the reasons: [`docs/design.md`](docs/design.md).

## What does not work yet

- **Nothing is released.**
  [`config/default`](config/default) deploys the manager, and ko builds its image: `kustomize build config/default | ko resolve -f -`.
  No image is published and there are no release artifacts.
- **A clusterctl install does not deliver the grant** that lets the kubeadm bootstrap provider read a `LodiaControlPlane`, which it needs for files with `contentFormat: Template` and, with v1.14.0 to v1.15.0-beta.0, for every join ([why](docs/design.md#what-the-provider-writes)).

## Manager flags

The manager takes the flags of [Cluster API's kubeadm control plane provider](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L128-L217) that apply to it, under their names and with their defaults; `--help` lists them.
It logs through klog: `--v` sets the verbosity, `--logging-format` the format, and, unless `--insecure-diagnostics` is set, `/debug/flags/v` on the diagnostics address changes the level while it runs.
`--namespace` limits the manager to one namespace.
`--watch-filter` limits it to the events of objects that carry the label `cluster.x-k8s.io/watch-filter` with that value.
Put the label on a Cluster and on its `LodiaControlPlane`.
The events of a Cluster and of a `LodiaControlPlane` count by their own labels, and those of Lodia's `ControlPlane` and Secrets by the Cluster's.
A `LodiaControlPlane` without the label is not reconciled on its own events: only its Cluster's events reach it, which are a change of the Cluster's spec, each start of the manager (it lists every Cluster again, and each gives one pass) and the Cluster's creation when the `LodiaControlPlane` is already in the cache.
A labelled `LodiaControlPlane` whose Cluster has none misses the events of Lodia's objects, and waits for its own next change or for the resync.
`--lodiacontrolplane-concurrency` (default 10) sets how many `LodiaControlPlane`s are reconciled at once.
The manifest passes `--leader-elect`, `--diagnostics-address` and `--insecure-diagnostics`; the last two take the clusterctl variables `CAPI_DIAGNOSTICS_ADDRESS` (default `:8443`) and `CAPI_INSECURE_DIAGNOSTICS` (default `false`), which [Cluster API documents for every provider](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/user/guide/providers/observability.md?plain=1#L7-L37).

Several instances of the provider from the released YAML are not supported.
A second install rewrites the subjects of the ClusterRoleBindings it shares with the first, which takes the first instance's grants away ([clusterctl](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L366)), and the leader election Lease has a fixed name, so every instance needs a manager namespace of its own.
`--namespace` and `--watch-filter` are there because the clusterctl contract asks for them and cluster-api-operator can pass them; Cluster API itself gives several instances [best-effort support only](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/core/support-multiple-instances.md?plain=1#L22-L37).

### Under cluster-api-operator

The operator writes the fields of a provider's `spec.manager` as flags of the manager ([customizer](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/component_customizer.go#L198-L421)).
`--watch-filter` and the `--tls-*` flags have no field of their own and reach the manager only through `spec.manager.additionalArgs` or the Deployment's args.

#### Fields that work

- `controller.groupKindConcurrency`
  Only with the key `LodiaControlPlane`: the operator writes `--<key>-concurrency` in lower case.
- `cacheNamespace`
- `metrics.diagnosticsAddress`
  The container's port `metrics` stays 8443, so a Service or scrape config that names it follows only while the address uses that port.
- `metrics.insecureDiagnostics`
- `profilerAddress`
- `leaderElection.leaderElect`
  The operator's [CRD](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/config/crd/bases/operator.cluster.x-k8s.io_controlplaneproviders.yaml#L3032-L3039) requires every field of `leaderElection`.
  With `leaderElect: true`, leave at least one of `leaderElection.resourceName` and `leaderElection.resourceNamespace` empty, or the operator writes `--leader-election-id` and the manager exits.
- `leaderElection.leaseDuration`
- `leaderElection.renewDeadline`
- `leaderElection.retryPeriod`
- `syncPeriod`
- `verbosity`
  The field's default, `1`, writes no flag and leaves the manager at `--v=2`, as in [Cluster API's kubeadm control plane provider](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L234-L237).
  `additionalArgs` with the key `--v` and the value `1` sets 1.
- `additionalArgs`
  For `--watch-filter` and the three `--tls-*` flags.

#### A field that works only as `:9440`

- `health.healthProbeBindAddress`
  The probes use the container's port `healthz`, 9440, which the operator never changes.
  Any other port, or a loopback host, fails the probes and restarts the pod.

#### Fields that make the manager exit at start

The operator writes a flag the manager does not define.

- `controller.groupKindConcurrency`
  With any other key, such as `LodiaControlPlane.controlplane.lodia.io`, which the field's own documentation asks for.
- `maxConcurrentReconciles`
  Cluster API's kubeadm control plane provider refuses it too.
- `metrics.bindAddress`
  Cluster API's kubeadm control plane provider refuses it too.
- `webhook.host`
  Cluster API's kubeadm control plane provider refuses it too.
- `leaderElection.resourceName`
  With `leaderElection.resourceNamespace` and `leaderElect: true` the operator writes `--leader-election-id`, which Cluster API's kubeadm control plane provider refuses too.
- `webhook.port`
  The provider has no webhooks; Cluster API's kubeadm control plane provider accepts it.
- `webhook.certDir`
  The provider has no webhooks; Cluster API's kubeadm control plane provider accepts it.
- `featureGates`
  The provider has no feature gates; Cluster API's kubeadm control plane provider accepts it.

#### Fields that break the probes

The operator writes no flag, and moves the probes off `/healthz` and `/readyz`, which the manager serves.

- `health.livenessEndpointName`
- `health.readinessEndpointName`

#### Fields the operator ignores

The operator writes no flag for them and changes nothing else ([`gracefulShutDown`](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/component_customizer.go#L234) and [`controller.cacheSyncTimeout`](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/component_customizer.go#L202) are marked TODO there).

- `gracefulShutDown`
- `controller.cacheSyncTimeout`
- `controller.recoverPanic`
- `leaderElection.resourceLock`
- `leaderElection.resourceNamespace`
  With `leaderElection.resourceName` empty; with both set and `leaderElect: true` it makes the manager exit, as listed above.

## Build and test

You need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.13.2.
Every other tool runs through `go tool` or `go run`; `make e2e` also needs what [its README](test/e2e/README.md) lists.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `config/crd/bases` and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, modules tidy, `config/default` rendered and checked for a non-empty default on every variable, one Namespace holding every namespaced object, the provider label on every object and a `manager` container as rendered, and for the roles that reach the manager's ServiceAccount on what clusterctl installs with the defaults and with explicit values, lint, unit and envtest tests |
| `make e2e` | the [end-to-end suite](test/e2e/README.md) on a kind cluster of its own; CI runs it |

The envtest suite starts its own API server and etcd; nothing in it reads a kubeconfig.

Licensed under the [Apache License 2.0](LICENSE).

[contract]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md
