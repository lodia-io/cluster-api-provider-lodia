# A LodiaControlPlane owns exactly one Lodia ControlPlane, named after the Cluster, and the provider creates no Machines

Decided 2026-10-26.

## Decision

- Each `LodiaControlPlane` owns exactly one Lodia `ControlPlane`, in its namespace and named after the `Cluster`, with a controller reference to the `LodiaControlPlane` and `blockOwnerDeletion`.
- The `LodiaControlPlane`'s spec reaches the `ControlPlane` by its JSON field names, but for `controlPlaneEndpoint`, which the provider writes alone, and the network, which comes from `Cluster.spec.clusterNetwork`.
  The field-by-field mapping is in [the reference](../README.md#lodiacontrolplane).
- The provider creates no Machines and never writes to the `Cluster` or the InfraCluster.
  `status.externalManagedControlPlane` is always `true`.
- Lodia's own `spec.paused` is never written: it stays a break-glass switch on the `ControlPlane` ([paused][lo-paused]).
- `LodiaControlPlaneTemplate` holds the same spec but `version`, `controlPlaneEndpoint` and `restoreFrom`.
- A name or a network that Lodia refuses is refused by Lodia's API server and reported as `Refused`, with the server's message, and nothing else is created ([conditions](../README.md#lodiacontrolplane)).

## Why

- Owner references never cross namespaces ([owners][k8s-owners]), so the `ControlPlane` sits in the `Cluster`'s namespace.
- With no control plane Machines, `externalManagedControlPlane` is what the Machine controller reads before it drains and deletes a worker's Node: with it unset, a deleted worker is neither drained nor its Node deleted ([rule][cp-extmanaged], [isDeleteNodeAllowed][m-allowed], [util.go][util-extmanaged]).
- The spec is converted, not copied field by field, so a field that the CRD test makes the two schemas share needs no code.
- The template leaves out what the `Cluster` or the provider sets: the topology controller writes `version` from `Cluster.spec.topology.version`, and leaves `controlPlaneEndpoint` alone while it is empty ([IgnorePaths][ct-ignore]).
- The InfraCluster's `status.initialization.provisioned` belongs to whoever manages the infrastructure ([infrastructure contract](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/infra-cluster.md?plain=1#L475-L490)).

## Rejected

- Naming the `ControlPlane` after the `LodiaControlPlane` — a `Cluster` has one `controlPlaneRef`, ClusterClass names that object `<cluster>-<random>`, and the name stays when the object is replaced.
- Writing to the InfraCluster, which Kamaji's provider does by default ([Kamaji][kp-patchcluster], [its feature gate][kp-gate]) and k0smotron's as an opt-in ([k0smotron][k0-infra]) — the field is the infrastructure manager's.
  Whether an opt-in returns is an [open question](../README.md#deliberately-not-offered--and-owed).

## Consequences

- The `Cluster`'s name is the `ControlPlane`'s, so it has to be one Lodia accepts: a DNS-1035 label of at most 32 characters ([what Cluster API needs from Lodia](../README.md#what-cluster-api-needs-from-lodia)).

## Code

- `internal/controller/lodiacontrolplane.go` — `applyControlPlane` builds the `ControlPlane` from the spec and the `Cluster`'s network; `report` copies the status.
- `api/v1alpha1/lodiacontrolplane_types.go` and `lodiacontrolplanetemplate_types.go` — the two kinds.
- `api/v1alpha1/crd_test.go` — `TestTheSpecPassesLodiasSchemaThrough`, `TestTheSpecHasNeitherTheNetworkNorLodiasPause` and `TestTheTemplateIsTheSpecWithoutWhatTheClusterSets`.
- `internal/controller/contract_test.go` — `TestAClusterGetsItsControlPlaneAndTheContract`, `TestEveryOptionalFieldReachesTheControlPlane` and `TestTheProviderLeavesLodiasBreakGlassPauseAlone`; `internal/controller/template_test.go` for the template.

<!-- Cluster API v1.15.0-beta.0 -->
[cp-extmanaged]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L589-L605
[ct-ignore]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/contract/controlplane.go#L130-L155
[m-allowed]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/machine/machine_controller.go#L827-L877
[util-extmanaged]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/util.go#L102-L110

<!-- controller-runtime, kubeadm, Kubernetes -->
[k8s-owners]: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/

<!-- the reference providers -->
[kp-patchcluster]: https://github.com/clastix/cluster-api-control-plane-provider-kamaji/blob/v0.21.0/controllers/kamajicontrolplane_controller_cluster_patch.go#L104-L151
[kp-gate]: https://github.com/clastix/cluster-api-control-plane-provider-kamaji/blob/v0.21.0/controllers/kamajicontrolplane_controller.go#L231-L233
[k0-infra]: https://github.com/k0sproject/k0smotron/blob/v2.3.0/internal/controller/controlplane/k0smotron_controlplane_controller.go#L888-L959

<!-- Lodia at 5737583 -->
[lo-paused]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/v1alpha1/controlplane_types.go#L124-L132
