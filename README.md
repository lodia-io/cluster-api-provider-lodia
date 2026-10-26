# cluster-api-provider-lodia

The [Cluster API](https://cluster-api.sigs.k8s.io/) control plane provider for
[Lodia](https://github.com/lodia-io/lodia), which runs Kubernetes control planes
as pods on a management cluster, one `ControlPlane` object per tenant cluster.

A `LodiaControlPlane` (`controlplane.lodia.io/v1alpha1`) owns exactly one Lodia
`ControlPlane`, named after the `Cluster` and in its namespace. The provider
passes the `LodiaControlPlane`'s spec through to it, takes the network from
`Cluster.spec.clusterNetwork`, and reports Lodia's status back in the form
Cluster API's [control plane contract][contract] v1beta2 reads. It creates no
Machines and never writes to the `Cluster` or the InfraCluster. Workers join
through Cluster API's kubeadm bootstrap provider. `LodiaControlPlaneTemplate`
holds the same spec, but the version, the endpoint and `restoreFrom`, for
ClusterClasses.

The design and its reasons: [`docs/design.md`](docs/design.md).

## The contract, mapped

| Cluster API reads | `LodiaControlPlane` | from Lodia's `ControlPlane` |
|---|---|---|
| `spec.version` ([contract][c-version]) | `spec.version`, `vMAJOR.MINOR.PATCH`, whose minor never decreases | written to `spec.version` |
| — | the rest of `spec` but `controlPlaneEndpoint` | written to `spec` as is; the CRD repeats Lodia's rules, and a test fails when the two schemas drift |
| `Cluster.spec.clusterNetwork` | — | written to `spec.network`: `services.cidrBlocks` → `serviceCIDRs`, `pods.cidrBlocks` → `podCIDRs`, `serviceDomain` → `clusterDomain` |
| `spec.controlPlaneEndpoint` ([contract][c-endpoint]) | written by the provider | `status.endpoint.apiServer` |
| `status.initialization.controlPlaneInitialized` ([contract][c-init]) | set the first time `Ready` is True, and kept | condition `Ready` |
| `status.version` ([contract][c-version]) | set once Lodia reports one | `status.kubernetes.version` |
| `status.externalManagedControlPlane` ([contract][c-external]) | always true: without it, Cluster API neither drains a deleted worker nor deletes its Node ([isDeleteNodeAllowed][m-allowed]) | — |
| condition `Available` ([contract][c-conditions]) | a mirror | condition `Reachable` |
| condition `Ready` | a mirror, unless the provider cannot write: `Refused` with the API server's message, `ObjectsControlledElsewhere` naming the object's controller, `Deleting` | condition `Ready` |
| condition `Paused` ([contract][c-pause]) | `Cluster.spec.paused` or the `cluster.x-k8s.io/paused` annotation; Lodia keeps running | — |
| Secret `<cluster>-kubeconfig` ([contract][c-kubeconfig]) | type `cluster.x-k8s.io/secret`, label `cluster.x-k8s.io/cluster-name`, key `value` | `<cp>-admin-kubeconfig`, key `kubeconfig` |
| Secret `<cluster>-ca` ([contract][c-certs]) | the same type and label, key `tls.crt` only | `<cp>-ca-bundle`, key `ca.crt` |
| CRD label `cluster.x-k8s.io/v1beta2: v1alpha1` ([contract][c-label]) | on both CRDs | — |
| full access to `controlplane.lodia.io` ([contract][c-group]) | [`config/rbac/aggregated_role.yaml`](config/rbac/aggregated_role.yaml), labelled `cluster.x-k8s.io/aggregate-to-manager: "true"` | — |
| `clusterctl move` | blocked: every `LodiaControlPlane` carries `clusterctl.cluster.x-k8s.io/block-move` ([annotation][move-ann]); a hosted control plane does not move | — |
| `spec.replicas` and its counters ([contract][c-replicas]) | none: Lodia's availability is a failure tolerance, not a number of instances | — |

Deleting a `LodiaControlPlane` deletes its `ControlPlane`; the provider's
finalizer, `controlplane.lodia.io/lodiacontrolplane`, goes only once the
`ControlPlane` is gone. When a `ControlPlane` or a Secret of the provider's name
exists with another controller, or none, the provider leaves it alone and says
so in `Ready`. Validation is CEL in the CRDs; there are no admission webhooks.

## What does not work yet

- **Workers cannot join through Cluster API v1.14.3.** Its kubeadm bootstrap
  provider looks up `<cluster>-ca` for a worker's join and requires a non-empty
  `tls.key` for it ([NewCertificatesForWorker][cabpk-worker-certs],
  [EnsureAllExist][cabpk-ensure], [joinWorker][cabpk-join]), although the
  join only hashes the certificates. The provider never copies the CA's private
  key, which stays in Lodia's PKI, and writes no placeholder.
  [kubernetes-sigs/cluster-api#14389](https://github.com/kubernetes-sigs/cluster-api/pull/14389)
  requires the key only where one is written; it is expected in v1.15, and we
  wait for it.
- **No worker is created until Lodia reports a version.** Without
  `status.version` the contract reads the control plane as provisioning
  ([IsProvisioning][ct-provisioning]), and the MachineSet preflight check
  `ControlPlaneIsStable`, on by default, holds every new Machine
  ([preflight][ms-preflight], [feature gate][feature]). Lodia writes
  `status.kubernetes.version` from its phase 2; until then `status.version`
  stays empty.
- **No end-to-end suite.** It needs Lodia installed on a management cluster
  from its Helm chart, which landed in
  [lodia-io/lodia#25](https://github.com/lodia-io/lodia/pull/25) on 2026-10-26
  and has no release yet; `make test` runs envtest only.

## Build and test

You need Go (the version in `go.mod`) and
[golangci-lint](https://golangci-lint.run/) v2.13.2. Every other tool runs
through `go tool`.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `config/crd/bases` and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, lint, unit and envtest tests |

The envtest suite starts its own API server and etcd; nothing in it reads a
kubeconfig.

Licensed under the [Apache License 2.0](LICENSE).

[contract]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md
[c-group]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L98-L144
[c-label]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L146-L184
[c-endpoint]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L237-L279
[c-replicas]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L309-L387
[c-version]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L389-L457
[c-external]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L606-L622
[c-init]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L658-L711
[c-conditions]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L740-L787
[c-kubeconfig]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L867-L886
[c-certs]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L888-L899
[c-pause]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L967-L971
[m-allowed]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/core/reconcilers/machine/machine_controller.go#L806-L856
[move-ann]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/cmd/clusterctl/api/v1alpha3/annotations.go#L35-L43
[cabpk-worker-certs]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/util/secret/certificates.go#L188-L200
[cabpk-ensure]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/util/secret/certificates.go#L271-L287
[cabpk-join]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L650-L692
[ct-provisioning]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/internal/contract/controlplane.go#L275-L293
[ms-preflight]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/core/reconcilers/machineset/machineset_preflight.go#L174-L181
[feature]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.3/feature/feature.go#L104
