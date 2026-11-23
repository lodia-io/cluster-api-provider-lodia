# The provider confirms the delete of Lodia's ControlPlane only for the deletion of its Cluster

Decided 2026-11-23.

## Decision

- The provider writes Lodia's annotation `lodia.io/deletion-confirmed: "true"` on the `ControlPlane` it owns in two cases only, each before the step that needs it:
  - while the `Cluster` that owns the `LodiaControlPlane` is being deleted, before the provider deletes the `ControlPlane`;
  - once that `Cluster` is gone, before the provider removes its finalizer and leaves the `ControlPlane` to the garbage collector.
- The confirmation is a merge patch that carries the UID the pass read, so a `ControlPlane` created since under the same name refuses it as a change of its UID ([objectmeta.go][k8s-uid]); a `ControlPlane` of another controller is never confirmed.
- A `LodiaControlPlane` deleted while its `Cluster` lives and is not being deleted gets no confirmation, and Lodia refuses the provider's delete.
  The provider keeps its finalizer and sets `Ready` and `Available` to `False`, `Deleting`, with a message that names the refusal, the remedies (delete the `Cluster`, or confirm the `ControlPlane` by hand) and the API server's answer, which holds the `kubectl annotate` that confirms it.
  The pass ends without an error, and the next one comes with an event of the `Cluster` or of the `ControlPlane`, or with the resync.
- A `LodiaControlPlane` without the owner reference waits for it, deleting or not, and confirms nothing.
- A paused `LodiaControlPlane` or `Cluster` holds the confirmation as it holds the rest of the deletion; once the `Cluster` is gone, only the `cluster.x-k8s.io/paused` annotation does.

## Why

- Lodia admits the delete of a `ControlPlane` only once confirmed, and refuses the garbage collector's delete of one whose owner is gone like any other ([who-is-trusted][lo-trusted], [Delete a cluster][lo-delete]).
- The gate is there for a wrong selector or a GitOps prune ([who-is-trusted][lo-trusted-why]), which deletes a `LodiaControlPlane` as readily as a `ControlPlane`, so confirming every delete of the provider's own object would leave the gate open one object up.
  The deletion of a `Cluster` is the act that Cluster API answers by deleting its control plane object ([deletion order][cl-delete]), so that is what the provider confirms.
- The Cluster controller keeps a `Cluster` until its control plane object is gone ([deletion order][cl-delete]), so a `Cluster` gone first was deleted, its finalizer removed by hand, and the garbage collector then deletes the `LodiaControlPlane` it owns.
- The confirmation comes before the finalizer goes, since a confirmation written after the collector's refusal does not wake it, and its next retry may come only after its backoff, up to 1000 seconds ([Delete a cluster][lo-delete]).
- The Cluster controller writes the owner reference on the control plane object on every pass of its `Cluster`, deleting or not ([phases][cl-always], [owner reference][cl-ownerref]), so a missing reference whose `Cluster` exists comes back.
  Until it does, the provider cannot tell which `Cluster`, if any, asked for the delete.
- A refusal holds until a person or the `Cluster` acts, and both reach the provider as events: the deletion of a `Cluster` changes its generation ([markAsDeleting][k8s-generation]), which the `Cluster` watch passes, and an annotation on the `ControlPlane` is an event of the `ControlPlane` watch.
  An error would only retry the refused delete with backoff and log it each time.

## Rejected

- Confirming every delete of the `LodiaControlPlane` — the gate would stop a prune of Lodia's `ControlPlane` and not one of the `LodiaControlPlane`.
- Confirming the `ControlPlane` when the provider creates it, which confirms every later delete since nothing clears the annotation ([Delete a cluster][lo-delete]) — the gate would be gone for every tenant of the provider.
- Releasing a deleting `LodiaControlPlane` that has no owner reference, as the provider did before the gate — with its `Cluster` alive, the garbage collector's delete would be refused and leave the `ControlPlane` without an owner and the `Cluster` without its control plane object.
- Confirming such a `LodiaControlPlane` — whoever can edit and delete it would delete the tenant and keep the `Cluster`.

## Consequences

- The deletion of a `Cluster` passes the gate, as Lodia's decision has the provider confirm it ([who-is-trusted][lo-trusted]): a prune that takes the `Cluster` takes its tenant.
- A `LodiaControlPlane` deleted alone does not go while its `Cluster` lives: it waits with `Deleting` and the remedy in its message ([what breaks](../README.md#what-breaks-and-how-it-shows)).
  Deleted with foreground propagation, it loses `<cluster>-kubeconfig` and `<cluster>-ca` meanwhile, which nothing writes again until the `Cluster` is deleted too ([what breaks](../README.md#what-breaks-and-how-it-shows)).
- A deleting `LodiaControlPlane` with neither the owner reference nor a `Cluster` waits for a person, who confirms the `ControlPlane` and removes the finalizer by hand.
- A live `clusterctl move` with `block-move` removed by hand leaves Lodia's `ControlPlane` in the source management cluster ([hosted-control-planes-do-not-move](hosted-control-planes-do-not-move.md#why)).
- Lodia admits a delete while the `ControlPlane`'s namespace is being deleted ([who-is-trusted][lo-trusted]), so deleting a `Cluster`'s namespace deletes its `ControlPlane` whatever the provider confirms.

## Code

- `internal/controller/lodiacontrolplane.go` — `reconcileDelete` confirms and deletes, or reports the refusal; `confirmTheGoneClustersControlPlane` and `confirm`; `Reconcile` waits for the owner reference.
- `internal/controller/suite_test.go` — the envtest installs Lodia's policies from `go.lodia.io/lodia/api/policies` beside its CRDs.
- `internal/controller/deletion_test.go` — `TestDeletingTheClusterConfirmsAndDeletesTheControlPlaneAndWaitsUntilItIsGone`, `TestALodiaControlPlaneDeletedAloneLeavesTheControlPlaneAndReportsTheRefusal`, `TestARefusedDeleteGoesOnOnceTheClusterIsDeletedOrTheControlPlaneConfirmed`, `TestTheControlPlaneIsConfirmedBeforeTheFinalizerGoesOnceTheClusterIsGone`, `TestAFailedConfirmationKeepsTheFinalizerOnceTheClusterIsGone` and `TestADeletedLodiaControlPlaneWithoutTheOwnerReferenceWaitsForIt`; `races_test.go` — `TestAControlPlaneReplacedBeforeTheConfirmationIsLeftAlone`; `pause_test.go` — `TestThePausedAnnotationHoldsDeletionOnceTheClusterIsGone`.
- `test/e2e` — the deletion of a `Cluster` against Lodia's chart, which refuses an unconfirmed delete ([the test](../../test/e2e/README.md#the-test)).

<!-- Cluster API v1.15.0-beta.0 -->
[cl-delete]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller.go#L410-L466
[cl-always]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller.go#L203-L216
[cl-ownerref]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_phases.go#L51-L66

<!-- Kubernetes v1.37.2 -->
[k8s-generation]: https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go#L1083-L1093
[k8s-uid]: https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apimachinery/pkg/api/validation/objectmeta.go#L358

<!-- Lodia at 2ab640f -->
[lo-trusted]: https://github.com/lodia-io/lodia/blob/2ab640f60e9148515823748adf5fdb28690fe286/docs/decisions/who-is-trusted.md?plain=1#L14-L18
[lo-trusted-why]: https://github.com/lodia-io/lodia/blob/2ab640f60e9148515823748adf5fdb28690fe286/docs/decisions/who-is-trusted.md?plain=1#L27-L28
[lo-delete]: https://github.com/lodia-io/lodia/blob/2ab640f60e9148515823748adf5fdb28690fe286/docs/operations.md?plain=1#L92-L109
