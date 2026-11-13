# cluster-api-provider-lodia

The [Cluster API](https://cluster-api.sigs.k8s.io/) control plane provider for [Lodia](https://github.com/lodia-io/lodia), which runs Kubernetes control planes as pods on a management cluster, one `ControlPlane` object per tenant cluster.

A `LodiaControlPlane` (`controlplane.lodia.io/v1alpha1`) owns exactly one Lodia `ControlPlane`, named after the `Cluster` and in its namespace.
The provider passes the `LodiaControlPlane`'s spec through to it, takes the network from `Cluster.spec.clusterNetwork`, and reports Lodia's status back in the form Cluster API's [control plane contract][contract] v1beta2 reads.
It creates no Machines and never writes to the `Cluster` or the InfraCluster.
Workers join through Cluster API's kubeadm bootstrap provider: [the e2e suite](test/e2e/README.md) joins a Docker machine to a cluster of this provider, and runs a pod on it.
`LodiaControlPlaneTemplate` holds the same spec, but the version, `controlPlaneEndpoint` and `restoreFrom`, for ClusterClasses.

How each rule of the contract is met, the life of the objects and the reasons: [`docs/design.md`](docs/design.md).

## Releases

The provider is released as pre-releases, `v0.1.0-alpha.1` and on, until Cluster API v1.15.0 is out and the provider's pin has moved to it, and then as `v0.1.0` ([`todo.md`](todo.md)).
A release is a [GitHub release](https://github.com/lodia-io/cluster-api-provider-lodia/releases) with the two files that clusterctl and cluster-api-operator read, `control-plane-components.yaml` and `metadata.yaml` ([the contract](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L84-L102)).
The manager's image is `ghcr.io/lodia-io/cluster-api-provider-lodia`, for linux/amd64 and linux/arm64, in a public package: pulling it takes no credentials.
`control-plane-components.yaml` names it as `<repository>:<tag>@sha256:<digest>`, which pulls by digest.
The pre-releases are unsigned; keyless signatures of the image and of both files come once the repository is public ([`todo.md`](todo.md)).
The repository is private for now, so clusterctl and cluster-api-operator need a token to read a release ([how](#reading-a-release-of-a-private-repository)).

## Install

The provider's watches need Lodia's CRDs, so install Lodia before the provider.
A release of the provider is built and tested against [Lodia at 57375832e7fe](https://github.com/lodia-io/lodia/tree/57375832e7fe8b3a06374613797eb4c544fe7612), the `LODIA_COMMIT` of the [`Makefile`](Makefile) and the pseudo-version of `go.lodia.io/lodia/api` in [`go.mod`](go.mod).
Lodia has no release yet, so that commit is the requirement of every release.

### The Cluster API version

A release of the provider needs Cluster API v1.15.0-beta.0, for the core provider and for the kubeadm bootstrap provider, until the provider's pin moves to v1.15.0 ([`todo.md`](todo.md)).
The provider copies Lodia's cluster CA into the Secret `<cluster>-ca` as `tls.crt` and writes no `tls.key` ([why](docs/design.md#43-the-ca-secret)).
A worker joins with such a Secret only since [kubernetes-sigs/cluster-api#14389](https://github.com/kubernetes-sigs/cluster-api/pull/14389), merged to `main` as [bccfd69](https://github.com/kubernetes-sigs/cluster-api/commit/bccfd69caf12e9703c2711d6d961ff99d6c0ebc3): `EnsureAllExist` asks for the key only of a certificate that has a key file, and a worker's CA certificate has none ([v1.15.0-beta.0](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/secret/certificates.go#L273-L288), [worker certificates](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/secret/certificates.go#L189-L200)).
v1.14.4, the newest stable release on 2026-11-13, asks for the key of every certificate that is not external, so the kubeadm bootstrap provider fails the join of every worker with `missing key data`, and no worker gets bootstrap data ([`EnsureAllExist`](https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.4/util/secret/certificates.go#L272-L287), [the join](https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.4/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L653-L679)).
The change is on `main` and in v1.15.0-beta.0 only: no backport to the v1.14 line existed on 2026-11-13.
Without a version, clusterctl and cluster-api-operator install the newest stable release ([clusterctl](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_versions.go#L86-L153), [operator](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/manifests_downloader.go#L89-L97)), so every command below names the version.

### With clusterctl

Add the provider to [clusterctl's configuration file](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/clusterctl/configuration.md?plain=1#L3), the way clusterctl [takes a provider outside its built-in list](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/clusterctl/configuration.md?plain=1#L10-L44):

```yaml
providers:
- name: lodia
  url: https://github.com/lodia-io/cluster-api-provider-lodia/releases/latest/control-plane-components.yaml
  type: ControlPlaneProvider
```

Use the name `lodia`: the manifests are labelled `cluster.x-k8s.io/provider: control-plane-lodia`, which is the label clusterctl [sets](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L543-L564) for that name.
Install Cluster API at [the version the provider needs](#the-cluster-api-version) and the provider with it:

```sh
clusterctl init --core cluster-api:v1.15.0-beta.0 --bootstrap kubeadm:v1.15.0-beta.0 --control-plane lodia --infrastructure <yours>
```

`--control-plane lodia` installs the newest release of the provider, and `--control-plane lodia:v0.1.0-alpha.1` pins a version.
A named control plane provider replaces the kubeadm one that `init` adds on its first run otherwise ([defaults](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/init.go#L275-L297)).
While every release is a pre-release, `latest` is the newest one; once a stable release exists, `latest` skips pre-releases ([selection](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_versions.go#L86-L153)).

The manifest names its image by digest.
An `images:` entry of clusterctl's configuration for `control-plane-lodia` with a `repository` moves the pull to a mirror and keeps the tag and the digest, so the mirror must hold the same image; a `tag` entry changes the tag and the digest still names the image ([overrides](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/clusterctl/configuration.md?plain=1#L215-L286), [how they apply](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/config/imagemeta_client.go#L58-L164), [the reference](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/container/image.go#L43-L81)).

### Variables

clusterctl reads a variable from the environment, and from its configuration file when the environment lacks it ([variables](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/clusterctl/configuration.md?plain=1#L46-L66)).

| variable | default | meaning |
|---|---|---|
| `LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE` | `capi-kubeadm-bootstrap-system` | the namespace of the kubeadm bootstrap provider's ServiceAccount |
| `LODIA_KUBEADM_BOOTSTRAP_SA_NAME` | `capi-kubeadm-bootstrap-manager` | its name |
| `CAPI_DIAGNOSTICS_ADDRESS` | `:8443` | where the manager serves diagnostics, see [Manager flags](#manager-flags) |
| `CAPI_INSECURE_DIAGNOSTICS` | `false` | whether it serves them without TLS and authentication |

The defaults of the first two are the namespace and the name that a clusterctl install of the kubeadm bootstrap provider gives its ServiceAccount ([namespace](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/default/kustomization.yaml#L1-L4), [name](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/rbac/service_account.yaml#L1-L5)); set them when yours differ.
The manifest binds that ServiceAccount's user, `system:serviceaccount:<namespace>:<name>` ([form](https://github.com/kubernetes/kubernetes/blob/v1.35.0/staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go#L53-L57)), to a ClusterRole that grants `get` on `lodiacontrolplanes` and nothing else.
The kubeadm bootstrap provider reads the `LodiaControlPlane` for files with `contentFormat: Template` and, at v1.15.0-beta.0, which the provider needs, for every join, while its own ClusterRole grants that for `kubeadmcontrolplanes` only ([why](docs/design.md#what-the-provider-writes)).
The binding names a user and not a ServiceAccount because clusterctl sets the namespace of every subject that has one to the provider's own ([rewrite](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L366)).

clusterctl keeps no variable's value in the cluster, and processes the variables again at every `clusterctl upgrade` ([inventory](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L120-L138), [upgrade](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L353-L373)).
A value given to `init` and not to `upgrade` falls back to its default, and the grant then names the default ServiceAccount.
Keep the values in clusterctl's configuration file, as top-level keys, on every machine that upgrades the management cluster.

### With cluster-api-operator

Install Cluster API at [the version the provider needs](#the-cluster-api-version): a `spec.version` on the core provider and on the kubeadm bootstrap provider, since the operator installs the newest stable release without one.

```yaml
apiVersion: operator.cluster.x-k8s.io/v1alpha2
kind: CoreProvider
metadata:
  name: cluster-api
  namespace: capi-system
spec:
  version: v1.15.0-beta.0
---
apiVersion: operator.cluster.x-k8s.io/v1alpha2
kind: BootstrapProvider
metadata:
  name: kubeadm
  namespace: capi-kubeadm-bootstrap-system
spec:
  version: v1.15.0-beta.0
```

Then the provider:

```yaml
apiVersion: operator.cluster.x-k8s.io/v1alpha2
kind: ControlPlaneProvider
metadata:
  name: lodia
  namespace: cluster-api-provider-lodia-system
spec:
  version: v0.1.0-alpha.1
  fetchConfig:
    url: https://github.com/lodia-io/cluster-api-provider-lodia/releases/latest/control-plane-components.yaml
  configSecret:
    name: lodia-variables
```

The operator takes the version from `spec.version` ([FetchConfiguration](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/api/v1alpha2/provider_types.go#L270-L298)).
The `url` has to be in clusterctl's full form, `https://github.com/{owner}/{repo}/releases/{latest|version}/control-plane-components.yaml`.
The `https://github.com/{owner}/{repo}/releases` that the operator's [docs](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/docs/book/src/03_topics/02_configuration/05_provider-spec-configuration.md?plain=1#L15-L24) show is refused by clusterctl's library, both [at v1.14.0](https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.0/cmd/clusterctl/client/repository/repository_github.go#L217-L225), which the operator [builds on](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/go.mod#L28), and [at v1.15.0-beta.0](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_github.go#L212-L225).
The operator reads variables only from the Secret that `spec.configSecret` names, in the provider's namespace, and uses them when it creates and upgrades the provider's components ([ConfigSecret](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/api/v1alpha2/provider_types.go#L54-L62), [reader](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/phase_initialize.go#L125-L176)): put the `LODIA_*` variables in it, and `github-token` while the repository is private.
[Under cluster-api-operator](#under-cluster-api-operator), below, says which fields of `spec.manager` the provider takes.

### Without clusterctl

`control-plane-components.yaml` holds clusterctl variables, which `kubectl apply` would leave as text.
Apply the output of `clusterctl generate yaml --from control-plane-components.yaml` instead: it substitutes the variables and does no more, so it sets no namespace, label or image ([generate yaml](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/config.go#L75-L111)).

### Reading a release of a private repository

Anyone with read access to a repository can view its releases ([GitHub](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases)), so while the repository is private clusterctl and the operator need a token that has it:
- clusterctl takes the token from `GITHUB_TOKEN` in the environment or `github-token` in its configuration file ([variable](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/config/reader_viper.go#L78-L88)); Cluster API's book asks for the `repo` scope ([book](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/clusterctl/overview.md?plain=1#L28-L40)).
  Every command that reads the repository needs it, `clusterctl upgrade` of any management cluster that has the provider included, whatever it upgrades ([upgrade info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)).
- The operator takes `github-token` from the Secret of `spec.configSecret`, and from nowhere else ([reader](https://github.com/kubernetes-sigs/cluster-api-operator/blob/v0.30.0/internal/controller/phase_initialize.go#L125-L176)).

## Upgrade

`clusterctl upgrade plan` never proposes a pre-release ([next version](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L166-L196)), so between pre-releases name the version: `clusterctl upgrade apply --control-plane lodia:v0.1.0-alpha.2` ([the `provider:version` form](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/upgrade.go#L283-L312)).
For the same reason a management cluster that runs an older Cluster API takes [the version the provider needs](#the-cluster-api-version) by name: `clusterctl upgrade apply --core cluster-api:v1.15.0-beta.0 --bootstrap kubeadm:v1.15.0-beta.0` ([flags](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/cmd/upgrade_apply.go#L78-L85)).
With cluster-api-operator, set `spec.version`, of the provider and of Cluster API's core and kubeadm bootstrap providers.
Keep the variables as [above](#variables) for either.
`upgrade plan` reads the repository of every provider of the management cluster, and fails when one has no entry in the configuration of the machine it runs on, so every machine that upgrades the management cluster needs the `providers:` entry ([provider info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)).

## `clusterctl move`

The provider refuses `clusterctl move`.
It writes the annotation `clusterctl.cluster.x-k8s.io/block-move` on every `LodiaControlPlane` in its first pass and never removes it, which makes a move wait for the annotation to go before it creates anything in the target ([contract](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L490-L543), [annotation](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/api/labels-and-annotations.md?plain=1#L40-L41)).
A hosted control plane is Lodia's pods, Services, etcd and Secrets in the source management cluster, under Lodia's `ControlPlane`, which move would not carry: it finds the kinds whose CRDs carry the label `clusterctl.cluster.x-k8s.io` ([discovery](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/objectgraph.go#L344-L430)), and Lodia's CRDs carry none ([CRD](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/crds/lodia.io_controlplanes.yaml#L1-L8)).

What the refusal costs:
- A live move pauses the Clusters and ClusterClasses, waits about six minutes for the annotation and fails ([move](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L337-L419), [wait](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L685-L743)).
- The Clusters stay paused, since clusterctl unpauses only at the end of a successful move ([unpause](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L416-L418)).
  Set `spec.paused` of each Cluster back to `false` and remove the annotation `cluster.x-k8s.io/paused` from each ClusterClass ([pause](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L593-L645)).
- A move takes one namespace and has no filter for a Cluster, so the annotation blocks the move of every Cluster in the namespace ([flags](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/cmd/move.go#L75-L92), [namespace](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/move.go#L85-L102)).
- `--dry-run` reports success, since it skips the wait.

Never remove `block-move` by hand to get a move through.
The move then deletes the source `LodiaControlPlane` ([delete](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L1253-L1314)), the garbage collector deletes Lodia's `ControlPlane` with it, and its `lodia.io/datastore` finalizer ends with the etcd deleted ([finalizer](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/v1alpha1/labels.go#L79-L81)).
The copy in the target is refused as below, so the Cluster would have a control plane in neither management cluster.
A paused `LodiaControlPlane` is not reconciled, so the provider cannot put the annotation back during a move.

The provider also refuses a copy of a `LodiaControlPlane`.
It records the object's own UID in the annotation `controlplane.lodia.io/uid` and compares the annotation with the object's UID at the start of every pass, before it does anything else.
A move and a restore keep annotations and give the copy a new UID, so the copy names another UID: the provider builds no `ControlPlane` and no Secret for it, writes nothing else to it, and sets `Ready` and `Available` to `False` with the reason `CopyOfAnotherObject`.
That covers what `block-move` does not stop: `clusterctl move --to-directory` followed by `--from-directory`, which never read the annotation ([to directory](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L421-L473), [from directory](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L475-L515)), and a restore from a backup of the Cluster API objects.
To give the copy a new, empty control plane, remove its annotation `controlplane.lodia.io/uid`: the provider then builds a new `ControlPlane`, with a new CA and an empty etcd, and writes `<cluster>-kubeconfig` and `<cluster>-ca` anew.
An object created from a manifest carries no such annotation, so a Cluster recreated from GitOps is not refused.

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

## Release a version

A tag `vMAJOR.MINOR.PATCH[-PRERELEASE]` is a release: pushing it runs [`release.yml`](.github/workflows/release.yml), which checks the tag, pushes the image, renders `control-plane-components.yaml` and publishes the GitHub release with it and `metadata.yaml`.
A tag with a pre-release part makes a pre-release.

1. A tag that starts a new minor needs the minor's series in `metadata.yaml`, added in a pull request before the tag, with the contract that the CRDs carry.
   `metadata.yaml` is append-only: `clusterctl upgrade plan` reads the newest release's file for every version of the provider that a management cluster may run, and fails for the whole management cluster when a series is missing ([upgrade info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)).
   `make metadata`, which `make verify` runs, refuses a series that the merge base with `origin/main` has and the file lacks, a series whose contract changed, and a newest series whose contract is not the CRDs' contract label.
2. Wait for the CI of `main` to finish on the commit: a tag pushed before it ends is refused.
3. `make release-check TAG=vX.Y.Z` (and `COMMIT=<sha>` for a commit other than `HEAD`) fetches `origin/main` and the tags, and refuses:
   - a tag that is not written `vMAJOR.MINOR.PATCH[-PRERELEASE]`;
   - a tag whose minor has no series in `metadata.yaml` at the commit, and any tag of the repository whose minor has none there;
   - a commit that is not on `origin/main`;
   - a commit whose runs of `ci.yml` for the push to `main` are missing, unfinished or not green.
   It reads GitHub with `GITHUB_TOKEN`, or with `gh auth token`.
4. Tag the commit and push the one tag by name: `git tag -s -m vX.Y.Z vX.Y.Z <sha>`, then `git push origin vX.Y.Z`.
   GitHub starts no workflow when more than three tags are pushed at once ([GitHub](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#push)), and such a tag gets no run, no release and no failed job.
   The workflow runs the same check again before it builds anything.
5. The workflow pushes the image for both platforms, renders `control-plane-components.yaml` with it, pulls the image by its digest without credentials, and then creates the release with `gh release create --verify-tag`, with generated notes and, for a pre-release tag, the pre-release flag.
   If the release job does not succeed, the `cleanup` job deletes the draft releases of the tag.

Between the push of a tag and the publication of its release, once the repository is public, `clusterctl upgrade plan` fails for every management cluster that has the provider: clusterctl takes the versions from proxy.golang.org, which lists a tag whether or not it has a release, and then asks for the files of the newest one ([versions](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_github.go#L104-L146), [proxy](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/goproxy/goproxy.go#L59-L143), [upgrade info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)).
The window is the run of the release job, which builds and publishes nothing before the tag.
While the repository is private, the proxy cannot list it and clusterctl lists releases, not tags, so there is no window.

### When a run fails

- A cause outside the commit, such as the package's visibility, the registry or a permission, is answered by re-running the failed job on the same tag.
  A re-run replays the tag's own commit, so it runs that commit's `release.yml`, `Makefile` and check.
- A cause inside the commit, in `release.yml`, the `Makefile` or the check, is fixed on `main`.
  Then delete the tag and tag the fixed commit again, or tag the next pre-release.
  Let the first run end before deleting its tag: `gh release create` names the tag, so a run still building publishes the assets of its own commit under a tag pushed again, and stays green.
- Once the repository is public, a tag that the workflow refuses or fails on anyway is answered at once by tagging the next pre-release of the same line, `v0.1.0-alpha.2` after `v0.1.0-alpha.1`, and never a stable release before `v0.1.0` is due.
  Delete the bad tag too, but nothing relies on that, since whether the proxy stops listing a deleted tag is not known.
- Before the change that makes the repository public (it lives in lodia-io/terraform), every semver tag must have a release, or the proxy would list a newest tag without one from the first public day; [`todo.md`](todo.md) has the check and its place among the steps.

### The first release

The first push to ghcr.io creates the package private, so the run is expected to stop at the pull of the image without credentials, before it creates the release.
Check first that the organization allows public packages: organization settings, Packages, "Package Creation" ([GitHub](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility#package-creation-visibility-for-organization-members)).
Then make the package public once, in its settings under "Danger Zone", which cannot be undone ([GitHub](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility#configuring-visibility-of-packages-for-an-organization)), and re-run the failed job on the same tag.

### Signing

The pre-releases are unsigned.
Keyless signing needs `id-token: write` in `release.yml` ([GitHub](https://docs.github.com/en/actions/concepts/security/openid-connect)): a later pull request adds it, after the change that makes the repository public and before the first stable tag ([`todo.md`](todo.md)).

## Build and test

You need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.13.2.
Every other tool runs through `go tool` or `go run`; `make e2e` also needs what [its README](test/e2e/README.md) lists.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `config/crd/bases` and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, modules tidy, `config/default` rendered and checked for a non-empty default on every variable, one Namespace holding every namespaced object, the provider label on every object and a `manager` container as rendered, and for the roles that reach the manager's ServiceAccount on what clusterctl installs with the defaults and with explicit values, `metadata.yaml` against the CRDs' contract label and the merge base with `origin/main` (fetch it first), the release's render with no push, lint, unit and envtest tests; the render has ko build both platforms and pull the base image from gcr.io, which takes minutes when the Go caches are cold |
| `make e2e` | the [end-to-end suite](test/e2e/README.md) on a kind cluster of its own; CI runs it |
| `make release-components` | `config/default` with the manager's image built by ko for `PLATFORMS`, pushed to `IMAGE_REPOSITORY` unless `KO_PUSH=false` and named by digest, written to `out/control-plane-components.yaml`; the release and the e2e use it |
| `make release-image` | prints the image that `out/control-plane-components.yaml` names, after requiring it to be the only one and to read `IMAGE_REPOSITORY:IMAGE_TAG@sha256:<digest>` |
| `make release-check TAG=vX.Y.Z` | refuses a tag before it is pushed, as [the release steps](#release-a-version) say |

The envtest suite starts its own API server and etcd; nothing in it reads a kubeconfig.

Licensed under the [Apache License 2.0](LICENSE).

[contract]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md
