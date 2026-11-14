# The provider runs no admission webhooks: every rule is CEL in the CRD schema, and the rules of Lodia's spec are repeated there

Decided 2026-10-26.

## Decision

- The contract asks for no webhook, and every rule of the provider's CRDs is CEL in the schema, as in Lodia ([CLAUDE.md][lo-claude], [Lodia's decision](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/no-admission-webhooks.md)).
- The pass-through fields reuse the nested types of `go.lodia.io/lodia/api`, so their CEL comes along.
  The rules that Lodia puts on `ControlPlaneSpec` and its fields (required, immutable, set at creation only, the `Compact` exclusions) are repeated in the provider's CRD, and `api/v1alpha1/crd_test.go` fails when the two schemas drift, as Lodia's own [`etcddruid_test.go`][lo-etcddruid-test] does for etcd-druid's.
- Rules that name `spec.network` stay Lodia's, since the network comes from the `Cluster`: Lodia's API server refuses the `ControlPlane`, and the provider reports `Refused`.

## Why

- What a webhook costs, and what CEL in the schema replaces, is Lodia's reasoning, which this decision shares ([Lodia's decision](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/no-admission-webhooks.md)).
- A rule that names `spec.network` could not be repeated in the provider's CRD, which has no `network`: the `Cluster` supplies it.

## Consequences

- A network or a name that Lodia refuses is accepted by the `LodiaControlPlane` and refused when the provider applies the `ControlPlane`; the condition `Refused` carries the server's message.
- A move of the pin of `go.lodia.io/lodia/api` that changes Lodia's spec changes the provider's CRDs: `make generate` writes them, and `make verify` fails until the result is committed.

## Code

- `api/v1alpha1/lodiacontrolplane_types.go` and `lodiacontrolplanetemplate_types.go` — the markers that make the CRDs; `config/crd/bases` is generated from them.
- `api/v1alpha1/crd_test.go` — `TestTheSpecPassesLodiasSchemaThrough` compares the two schemas but for `network`, `paused` and the rules that name `self.network`; `TestTheTemplateIsTheSpecWithoutWhatTheClusterSets`.
- `internal/controller/report_test.go` — `TestANameLodiaRefusesIsReportedAndNothingIsCreated`; `internal/controller/template_test.go` — `TestATemplateWithCompactAndHighAvailabilityIsRefused`.

<!-- Lodia at 5f212ed -->
[lo-claude]: https://github.com/lodia-io/lodia/blob/5f212ed9acd71fe2a944e5fe63cad73e3d17812e/CLAUDE.md?plain=1#L53-L54
[lo-etcddruid-test]: https://github.com/lodia-io/lodia/blob/5f212ed9acd71fe2a944e5fe63cad73e3d17812e/api/v1alpha1/etcddruid_test.go
