# cluster-api-provider-lodia

The [Cluster API](https://cluster-api.sigs.k8s.io/) control plane provider for
[Lodia](https://github.com/lodia-io/lodia), which runs Kubernetes control planes
as pods on a management cluster, one `ControlPlane` object per tenant cluster.

A `LodiaControlPlane` (`controlplane.lodia.io/v1alpha1`) owns exactly one Lodia
`ControlPlane`, named after the `Cluster` and in its namespace. The provider
passes the `LodiaControlPlane`'s spec through to it, takes the network from
`Cluster.spec.clusterNetwork`, and reports Lodia's status back in the form
Cluster API's [control plane contract][contract] v1beta2 reads. It creates no
Machines and never writes to the `Cluster` or the InfraCluster.
Workers are to join through Cluster API's kubeadm bootstrap provider; no test has shown that yet ([below](#what-does-not-work-yet)).
`LodiaControlPlaneTemplate` holds the same spec, but the version, the endpoint and `restoreFrom`, for ClusterClasses.

The design and its reasons: [`docs/design.md`](docs/design.md).

## The contract, mapped

| Cluster API reads | `LodiaControlPlane` | from Lodia's `ControlPlane` |
|---|---|---|
| `spec.version` ([contract][c-version]) | `spec.version`, `vMAJOR.MINOR.PATCH`, whose minor never decreases | written to `spec.version` |
| — | the rest of `spec` but `controlPlaneEndpoint` | written to `spec` as is; the CRD repeats Lodia's rules, and a test fails when the two schemas drift |
| `Cluster.spec.clusterNetwork` | — | written to `spec.network`: `services.cidrBlocks` → `serviceCIDRs`, `pods.cidrBlocks` → `podCIDRs`, `serviceDomain` → `clusterDomain` |
| `spec.controlPlaneEndpoint` ([contract][c-endpoint]) | written by the provider | `status.endpoint.apiServer` |
| `status.initialization.controlPlaneInitialized` ([contract][c-init]) | set the first time `Ready` is True, and kept | condition `Ready` |
| `status.versions` and `status.version` ([contract][c-version]) | set once Lodia reports one; `versions` holds that one entry | `status.kubernetes.version` |
| `status.externalManagedControlPlane` ([contract][c-external]) | always true: without it, Cluster API neither drains a deleted worker nor deletes its Node ([isDeleteNodeAllowed][m-allowed]) | — |
| condition `Available` ([contract][c-conditions]) | a mirror at any generation; `False`, `ControlPlaneNotFound` when an initialized `ControlPlane` is gone | condition `Reachable` |
| condition `Ready` | a mirror at the `ControlPlane`'s current generation, otherwise `Unknown`, `LatestGenerationNotObserved`; unless the provider cannot write: `Refused` with the API server's message, `ObjectsControlledElsewhere` naming the object's controller, `ControlPlaneNotFound`, `Deleting` | condition `Ready` |
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
so in `Ready`.
A `ControlPlane` deleted behind the provider's back after initialization is not recreated, since a new one would start with a new CA and an empty etcd.
Validation is CEL in the CRDs; there are no admission webhooks.

`clusterctl move` always fails on the block-move annotation, and the Clusters it paused before the wait stay paused ([move][move-pause]): set each one's `spec.paused` back to `false`.

The provider builds on Cluster API `v1.15.0-beta.0`, and moves to v1.15.0 when that is released, planned for 2026-12-08 ([release plan][release-plan]).

## What does not work yet

- **No worker has joined in a test.**
  The two conditions are met on paper: the kubeadm bootstrap provider takes a `<cluster>-ca` with `tls.crt` alone ([EnsureAllExist][cabpk-ensure]), and Lodia reports the version that the MachineSet preflight check `ControlPlaneIsStable` waits for ([IsProvisioning][ct-provisioning], [preflight][ms-preflight]).
  `make test` calls that bootstrap provider's certificate check on the Secret the provider writes, and nothing beyond it.
- **No end-to-end suite.**
  Lodia's Helm chart exists, and the suite is not written; `make test` runs envtest only.

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

[contract]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md
[c-group]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L95-L141
[c-label]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L143-L176
[c-endpoint]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L229-L270
[c-replicas]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L298-L373
[c-version]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L375-L443
[c-external]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L589-L605
[c-init]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L641-L691
[c-conditions]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L715-L759
[c-kubeconfig]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L836-L855
[c-certs]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L857-L868
[c-pause]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L928-L932
[m-allowed]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/machine/machine_controller.go#L827-L877
[move-pause]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L355-L377
[release-plan]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/release/releases/release-1.15.md?plain=1#L24
[move-ann]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/api/v1alpha3/annotations.go#L35-L43
[cabpk-ensure]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/secret/certificates.go#L271-L288
[ct-provisioning]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/contract/controlplane.go#L275-L293
[ms-preflight]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/machineset/machineset_preflight.go#L181-L188
