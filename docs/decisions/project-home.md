# cluster-api-provider-lodia is an Apache-2.0 project with the Go module go.lodia.io/cluster-api-provider-lodia, the API group controlplane.lodia.io and the clusterctl name lodia

Decided 2026-10-26.

## Decision

- **Home:** `github.com/lodia-io/cluster-api-provider-lodia`.
  Issues and pull requests live there, and CI runs on GitHub Actions.
- **Go module:** `go.lodia.io/cluster-api-provider-lodia`, a path of Lodia's own domain, as Lodia's `go.lodia.io/lodia`.
  It imports `sigs.k8s.io/cluster-api/api` (its own module since v1.14) and `go.lodia.io/lodia/api` ([go.mod][lo-api-gomod]).
- **API:** group `controlplane.lodia.io`, version `v1alpha1`, kinds `LodiaControlPlane` and `LodiaControlPlaneTemplate`; the CRDs are `lodiacontrolplanes.controlplane.lodia.io` and `lodiacontrolplanetemplates.controlplane.lodia.io`.
  The finalizer and the annotations are named under the group: `controlplane.lodia.io/lodiacontrolplane` and `controlplane.lodia.io/uid`.
- **clusterctl:** the provider is named `lodia`, so every manifest carries the label `cluster.x-k8s.io/provider: control-plane-lodia`, and the variables it defines start with `LODIA_`.
- **License:** Apache License 2.0, and contributions under the Developer Certificate of Origin (DCO sign-off), as Lodia's.
  Every Go file starts with Lodia's short Apache-2.0 header.
- What the provider shares with Lodia, the license, the DCO and the domain of the Go module, is Lodia's [project-home](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/project-home.md).

## Why

- A group other than `controlplane.cluster.x-k8s.io` keeps the provider's kinds apart from Lodia's `lodia.io`, and the contract names a control plane CRD `<plural>.<group>` ([definition][cp-crd], [CalculateCRDName][crd-name]) with `<env>ControlPlane` as the kind's convention.
- clusterctl labels the manifests of a provider with `cluster.x-k8s.io/provider: control-plane-<name>` for the name it is configured under ([label][cc-label]), and the contract's rules for the built-in list prefix the organization's name and drop the prefix where it would stutter, which gives `lodia` ([names][cc-names]).

## Rejected

- The group `lodia.io` with the kind `ControlPlane` — the provider's kinds would sit among Lodia's, under the name of one of them.

## Consequences

- The repository carries `LICENSE`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md` and `MAINTAINERS.md`.
- A group other than Cluster API's needs the aggregated ClusterRole that gives Cluster API's core controllers access to the kinds ([group][cp-group]), which `config/rbac/aggregated_role.yaml` is.
- A name other than `lodia`, if clusterctl's built-in list asks for another, needs the label in `config/default/kustomization.yaml` changed with it ([`todo.md`](../../todo.md)).

## Code

- `go.mod` — the module path and the two imports.
- `api/v1alpha1/groupversion_info.go` — the group and the version; `api/v1alpha1/lodiacontrolplane_types.go` has the finalizer and the annotation names.
- `config/default/kustomization.yaml` — the provider label.
- `hack/boilerplate.go.txt` — the header of every Go file, which `make generate` puts on the generated ones too.
- `LICENSE`.

<!-- Cluster API v1.15.0-beta.0 -->
[cp-group]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L95-L141
[cp-crd]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L178-L227
[crd-name]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/contract/contract.go#L28-L30
[cc-label]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L543-L564
[cc-names]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L34-L82

<!-- Lodia at 5737583 -->
[lo-api-gomod]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/go.mod#L1-L10
