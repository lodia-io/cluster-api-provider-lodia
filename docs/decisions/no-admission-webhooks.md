# The provider runs no admission webhooks: every rule is CEL in the CRD schema, and the rules of Lodia's spec are repeated there

Decided 2026-10-26.

## Decision

- The contract asks for no webhook, and every rule of the provider's CRDs is CEL in the schema, as in Lodia ([CLAUDE.md][lo-claude], [Lodia's decision](https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/docs/decisions/no-admission-webhooks.md)).
- The pass-through fields reuse the nested types of `go.lodia.io/lodia/api`, so their CEL comes along.
  The rules that Lodia puts on `ControlPlaneSpec` and its fields (required, immutable, set at creation only, set only where `apiserver-proxy` runs, the replicas of `highAvailability`, a JWT issuer that kube-apiserver refuses) are repeated in the provider's CRD, and `api/v1alpha1/crd_test.go` fails when the two schemas drift, as Lodia's own [`etcddruid_test.go`][lo-etcddruid-test] does for etcd-druid's.
- Rules that name `spec.network` stay Lodia's, since the network comes from the `Cluster`: Lodia's API server refuses the `ControlPlane`, and the provider reports `Refused`.

## Why

- What a webhook costs, and what CEL in the schema replaces, is Lodia's reasoning, which this decision shares ([Lodia's decision](https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/docs/decisions/no-admission-webhooks.md)).
- A rule that names `spec.network` could not be repeated in the provider's CRD, which has no `network`: the `Cluster` supplies it.

## Consequences

- A network or a name that Lodia refuses, or an IP `endpoint.loadBalancer.host` or an advertise address that does not fit the network, is accepted by the `LodiaControlPlane` and refused when the provider applies the `ControlPlane`; the condition `Refused` carries the server's message.
  The provider's CRD repeats Lodia's rules that keep `endpoint.loadBalancer.host` and `apiServer.advertiseAddress` once set, so such a value cannot be corrected in place: the `LodiaControlPlane`, or its `Cluster`, is deleted and created again with the corrected value.
- A move of the pin of `go.lodia.io/lodia/api` that changes Lodia's spec changes the provider's CRDs: `make generate` writes them, and `make verify` fails until the result is committed.

## Code

- `api/v1alpha1/lodiacontrolplane_types.go` and `lodiacontrolplanetemplate_types.go` — the markers that make the CRDs; `api/crds` is generated from them ([where the CRDs come from](../README.md#the-api-module)).
- `api/v1alpha1/crd_test.go` — `TestTheSpecPassesLodiasSchemaThrough` compares the two schemas but for `network`, `paused` and the rules that name `self.network`; `TestTheTemplateIsTheSpecWithoutWhatTheClusterSets`.
- `internal/controller/report_test.go` — `TestANameLodiaRefusesIsReportedAndNothingIsCreated`; `internal/controller/template_test.go` — `TestATemplateWithHighAvailabilityAndOneAPIServerIsRefused`.

<!-- Lodia at f91168b -->
[lo-claude]: https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/CLAUDE.md?plain=1#L61-L63
[lo-etcddruid-test]: https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/test/api/etcddruid_test.go
