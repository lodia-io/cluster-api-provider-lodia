# The status mirrors Lodia's: Available follows Reachable, Ready follows Ready at the current generation, and there are no replica counts

Decided 2026-10-26.

## Decision

- `Available` mirrors Lodia's `Reachable` at any generation: its status, reason and message.
- `Ready` mirrors Lodia's `Ready` only at the `ControlPlane`'s current generation, and is `Unknown` with the reason `LatestGenerationNotObserved` otherwise.
- `status.initialization.controlPlaneInitialized` is set the first time Lodia's `Ready` is `True`, at any generation, on a pass that has copied both Secrets, and is never cleared.
- `status.versions` and `status.version` come from Lodia's `status.kubernetes.version`, once Lodia reports one; `versions` holds that one entry.
- There is no `spec.replicas`, no scale subresource, no `status.selector` and none of the counters.
- The provider's own reasons, the verdicts it writes where it cannot write, are in [the condition table](../README.md#lodiacontrolplane).

## Why

- `Available` is mirrored into the `Cluster`'s `ControlPlaneAvailable`, and the contract asks that it hold through disruption ([conditions][cp-conditions], [mirror][cs-available]).
- Lodia's `Ready` True at the object's generation means available with every change taken up ([Lodia's rule][lo-ref-ready]).
- The initialization latch is kept as Cluster API keeps the `Cluster`'s ([Cluster controller][cl-cp]), and the contract reads it ([initialization][cp-init]).
- Without a version no worker is ever created, since the MachineSet's preflight holds every new Machine while the control plane reports none ([what Cluster API needs](../README.md#a-version-in-status)).
  Lodia's `status.kubernetes.version` is the oldest version any part of the control plane runs ([field reference][lo-ref-kubernetes]).
- Replica counts belong to the contract only together with `spec.replicas`, the scale subresource, `status.selector`, the four counters and a replica count per version entry ([replicas][cp-replicas], [version][cp-version]).
  Lodia's availability is a failure tolerance (`highAvailability`) with replica counts derived from it, not one number of instances.

## Rejected

- Gating `Available` on the generation, as `Ready` is — `Reachable` probes the live endpoint, which a change to the spec does not take down, and the contract asks that `Available` hold through disruption.
- The MachineSet preflight's skip annotation, to start workers before Lodia reports a version — it also turns off the gating of upgrades.

## Consequences

- Cluster API leaves the `Cluster`'s control plane counters empty ([setControlPlaneReplicas][cs-replicas]).
- A topology or a ClusterClass must not set what the missing replica fields stand for; [the contract table](../README.md#the-contract-as-built) lists it.

## Code

- `internal/controller/lodiacontrolplane.go` — `report` writes the conditions, the initialization and the versions.
- `internal/controller/report_internal_test.go` — `TestTheControlPlaneStatusIsReportedToTheLodiaControlPlane`.
- `internal/controller/report_test.go` — `TestReadyIsMirroredOnlyAtTheCurrentGeneration`, `TestReadyAtAnOlderGenerationInitializes`, `TestInitializationWaitsForBothSecrets`, `TestAControlPlaneWithNoStatusIsNotYetReported` and `TestTheVersionIsLodiasAndFollowsItPastTheFirstCopy`.

<!-- Cluster API v1.15.0-beta.0 -->
[cp-replicas]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L298-L373
[cp-version]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L375-L443
[cp-init]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L641-L691
[cp-conditions]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L715-L759
[cl-cp]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_phases.go#L251-L350
[cs-replicas]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_status.go#L121-L175
[cs-available]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_status.go#L450-L494

<!-- Lodia at 5737583 -->
[lo-ref-ready]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/api/controlplane.md?plain=1#L131
[lo-ref-kubernetes]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/api/controlplane.md?plain=1#L77
