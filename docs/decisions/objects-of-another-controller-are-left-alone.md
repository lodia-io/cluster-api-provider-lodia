# The provider leaves alone every object another controller owns, and never rebuilds a ControlPlane that an initialized LodiaControlPlane has lost

Decided 2026-10-26.

## Decision

- Before each apply the provider reads the object from the API server, past the cache.
  When the `ControlPlane`, `<cluster>-kubeconfig` or `<cluster>-ca` exists with another controller, or none, the provider leaves it alone and reports `ObjectsControlledElsewhere`, which names it.
  It never adopts a predecessor's object.
- It copies a source Secret only when the `ControlPlane` controls it.
- Once the `LodiaControlPlane` is initialized, a `ControlPlane` that is gone is not created again: the provider reports `ControlPlaneNotFound` and ends the pass.
  The apply carries the UID that the read found, so a `ControlPlane` gone in between fails the apply, and the next pass reports it.
- Deletion leaves a `ControlPlane` of another controller alone.

## Why

- A `Cluster` that comes from another control plane provider has that provider's objects: Kamaji's own `<cluster>-kubeconfig` and `<cluster>-ca` ([Kamaji][kp-ca]), and the provider reports them as `ObjectsControlledElsewhere` until they go.
- A predecessor's Secrets that await the garbage collector stay out of the contract's.
- A cache that lags behind the API server could hide a leftover `ControlPlane` or Secret, so the decisions about another controller's objects are read from the API server.

## Rejected

- Creating a new `ControlPlane` for an initialized `LodiaControlPlane` whose own is gone — a new one would start with a new CA and an empty etcd, and overwrite `<cluster>-ca` and `<cluster>-kubeconfig`.

## Consequences

- An owner reference removed from a `ControlPlane` or a Secret that the provider does not control stays removed: the provider would not put it back, so the e2e's pause checks leave such objects alone ([`test/e2e/README.md`](../../test/e2e/README.md)).

## Code

- `internal/controller/lodiacontrolplane.go` — `currentControlPlane`, `initialized`, `copySecrets`, `applySecret` and `reconcileDelete` read through `APIReader`; `applyControlPlane` applies with the UID that `currentControlPlane` read; `reportLost` and `controlledElsewhereError` carry the two reasons.
- `internal/controller/foreign_test.go` — `TestALeftoverControlPlaneOfAnotherOwnerIsLeftAlone`, `TestAPredecessorsSecretsAreNotCopied`, `TestALeftoverSecretOutsideTheCacheIsLeftAlone` and `TestDeletionLeavesAForeignControlPlaneAlone`.
- `internal/controller/races_test.go` — `TestAnInitializedControlPlaneIsNotRecreated`, `TestAControlPlaneVanishingBeforeTheApplyIsNotRecreated` and `TestAStaleCacheDoesNotHideALeftoverControlPlane`.

<!-- the reference providers -->
[kp-ca]: https://github.com/clastix/cluster-api-control-plane-provider-kamaji/blob/v0.21.0/controllers/kamajicontrolplane_controller_resources.go#L61-L124
