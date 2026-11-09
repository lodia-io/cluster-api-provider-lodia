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

## Build and test

You need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.13.2.
Every other tool runs through `go tool` or `go run`; `make e2e` also needs Docker and helm.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `config/crd/bases` and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, lint, unit and envtest tests |
| `make e2e` | the [end-to-end suite](test/e2e/README.md) on a kind cluster of its own; CI runs it, and it needs a route to Docker's network `kind` |

The envtest suite starts its own API server and etcd; nothing in it reads a kubeconfig, and the e2e suite reads only the kubeconfig of its own kind cluster.

Licensed under the [Apache License 2.0](LICENSE).

[contract]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md
