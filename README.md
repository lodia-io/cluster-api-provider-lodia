# cluster-api-provider-lodia

The [Cluster API](https://cluster-api.sigs.k8s.io/) control plane provider for [Lodia](https://github.com/lodia-io/lodia), which runs Kubernetes control planes as pods on a management cluster, one `ControlPlane` object per tenant cluster.
A `LodiaControlPlane` owns exactly one Lodia `ControlPlane`, passes its spec through to it, takes the network from the `Cluster`, and reports Lodia's status back in the form Cluster API's [control plane contract](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md) v1beta2 reads; it creates no Machines, and workers join through Cluster API's kubeadm bootstrap provider.

**Status: pre-releases, `v0.1.0-alpha.N`, until Cluster API v1.15.0 is out and the provider's pin has moved to it, and then `v0.1.0`.**

- [`docs/README.md`](docs/README.md) — what the provider is and does: the objects, how each rule of the contract is met, a pass, the manager, [installation](docs/README.md#installation), upgrading and `clusterctl move`.
- [`docs/decisions/`](docs/decisions/) — why it is built this way, one decision per file; [project-home](docs/decisions/project-home.md) has the module, the API group and the license.
- [`test/e2e/README.md`](test/e2e/README.md) — the end-to-end suite: what it installs, its steps and what each checks.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — building and testing, what CI runs, commits and cutting a release.
- [`CLAUDE.md`](CLAUDE.md) — how to work on the repository.
- [`SECURITY.md`](SECURITY.md) — how to report a vulnerability.
- [`todo.md`](todo.md) — findings and work waiting for triage.

Licensed under the [Apache License 2.0](LICENSE).
