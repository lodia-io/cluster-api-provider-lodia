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
`LodiaControlPlaneTemplate` holds the same spec, but the version, `controlPlaneEndpoint` and `restoreFrom`, for ClusterClasses.

How each rule of the contract is met, the life of the objects and the reasons: [`docs/design.md`](docs/design.md).

## What does not work yet

- **No worker has joined in a test.**
  The two conditions are met on paper: the kubeadm bootstrap provider takes a `<cluster>-ca` with `tls.crt` alone ([EnsureAllExist][cabpk-ensure]), and Lodia reports the version that the MachineSet preflight check `ControlPlaneIsStable` waits for ([IsProvisioning][ct-provisioning], [preflight][ms-preflight]).
  `make test` calls that bootstrap provider's certificate check on the Secret the provider writes, and nothing beyond it.
- **No end-to-end suite.**
  Lodia's Helm chart exists, and the suite is not written; `make test` runs envtest only.
- **Nothing can be deployed yet.**
  The repository has no image build and no manifests: no Dockerfile or ko config, no Deployment, ServiceAccount or ClusterRoleBinding, and no `kustomization.yaml`.

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
[cabpk-ensure]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/secret/certificates.go#L271-L288
[ct-provisioning]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/contract/controlplane.go#L275-L293
[ms-preflight]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/machineset/machineset_preflight.go#L181-L188
