# A hosted control plane does not move: clusterctl move is refused for good, and a copy of a LodiaControlPlane is never rebuilt

Decided 2026-10-26.

## Decision

- Every `LodiaControlPlane` carries `clusterctl.cluster.x-k8s.io/block-move`, written before the `ControlPlane` is created and never removed, so a move waits for the annotation to go before it creates anything in the target ([contract](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L490-L543), [annotation](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/reference/api/labels-and-annotations.md?plain=1#L40-L41)).
  Both CRDs carry the label `clusterctl.cluster.x-k8s.io`, which move finds CRDs by ([discovery][move-crds]), so the annotation is seen on an installation made by hand too.
- The provider records the UID the object had when it first reconciled it in the annotation `controlplane.lodia.io/uid`.
  The annotation is written only when absent, together with `block-move` and before the first apply of the `ControlPlane`, in one patch locked on the resourceVersion that the pass read; a pass whose patch did not land applies nothing.
- Every pass compares the annotation with the object's UID, and with nothing else, before every other step, the one that writes `block-move` included.
  A `LodiaControlPlane` that is not being deleted and whose annotation names another UID gets `Ready` and `Available` `False` with the reason `CopyOfAnotherObject`, and nothing else is written to it: neither a finalizer, a `Paused` condition, an annotation, a `ControlPlane` nor a Secret.
- A copy that is being deleted skips the refusal, so that its finalizer can go.
  Its deletion removes only its own finalizer, and a `ControlPlane` of the same name stays.
- An object created from a manifest never carries the annotation, so a `Cluster` recreated from GitOps is not refused.
  A user who wants a new, empty control plane for a copy removes the annotation.

## Why

- `clusterctl move` re-creates the objects elsewhere and deletes the source ones ([deleteSourceObject][move-delete]), and a hosted control plane is Lodia's pods, Services, etcd and Secrets in the source management cluster, under Lodia's `ControlPlane`, which move would not carry: it finds the kinds whose CRDs carry the label `clusterctl.cluster.x-k8s.io` ([discovery](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/objectgraph.go#L344-L430)), and Lodia's CRDs carry none ([CRD](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/crds/lodia.io_controlplanes.yaml#L1-L8)).
  The `ControlPlane` would follow its owner into garbage collection.
- The annotation is permanent because a paused `LodiaControlPlane` is not reconciled, so the provider could not put it back during a move.
  With it removed by hand, a live move deletes the source `LodiaControlPlane` ([delete](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L1253-L1314)), the garbage collector deletes Lodia's `ControlPlane`, and its `lodia.io/datastore` finalizer ends with the etcd deleted ([finalizer][lo-finalizer]).
  The refusal of a copy only keeps the target from building an empty control plane, so the `Cluster` has none in either management cluster.
- `block-move` is not enough: `move --to-directory` followed by `--from-directory` never read the annotation ([to directory](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L421-L473), [from directory](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L475-L515)), and neither does a restore from a backup of the Cluster API objects.
  A move and a restore keep annotations and give the copy a new UID.
  Without a refusal the provider would find no `ControlPlane` for the copy, build a new one with a new CA and an empty etcd, and overwrite both Secrets, silently.
- The comparison runs before the step that writes `block-move`, so a copy that a live move left after `block-move` was removed by hand, which carries the source's UID and no `block-move`, is refused like any other.
- A refused copy never deletes or adopts a `ControlPlane` or a Secret it did not create.
- Lodia's `lodia.io/sealed-uid` guards its Backups the same way ([sealed-uid][lo-sealed]).

## Rejected

- An unlocked patch of the annotations — a cache that lags behind could hide the UID that another object's copy has just been given, and the patch would replace it with this object's own, which the next pass would then trust.

## Consequences

- A refused move costs a wait, leaves the Clusters paused and blocks every Cluster of the namespace; [the reference](../README.md#clusterctl-move) says how to undo it.
- `block-move` is never removed by hand to get a move through.

## Code

- `internal/controller/lodiacontrolplane.go` — `reconcileNormal` and `annotate` write the two annotations; `refuseCopy` refuses a copy.
- `internal/controller/guard_test.go` — `TestTheUIDIsRecordedBeforeTheControlPlaneIsApplied`, `TestAPassWhoseAnnotationPatchFailsAppliesNothing`, `TestAPassThatReadAStaleCopyDoesNotWriteItsUIDOverAnotherObjects`, `TestACopyOfAnotherObjectGetsNeitherAControlPlaneNorSecretsNorARewrittenAnnotation`, `TestRemovingTheUIDAnnotationOfACopyBuildsAControlPlaneForIt` and `TestARefusedCopyCanBeDeletedAndLeavesTheControlPlaneOfItsNameAlone`.
- `internal/controller/move_test.go` — `TestClusterctlMoveWaitsForTheBlockOnTheLodiaControlPlane`; `api/v1alpha1/crd_test.go` — `TestTheCRDsCarryTheContractLabel`.
- `test/docs/names_test.go` — `TestTheDocsNameTheAnnotationAndTheReasonUsersActOn`.
- `test/e2e/README.md` — step 10, where `clusterctl move` ends at its wait for the annotation.

<!-- Cluster API v1.15.0-beta.0 -->
[move-crds]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/objectgraph.go#L420-L431
[move-delete]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L1260-L1312

<!-- Lodia at 5737583 -->
[lo-finalizer]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/v1alpha1/labels.go#L79-L81
[lo-sealed]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/v1alpha1/labels.go#L83-L88
