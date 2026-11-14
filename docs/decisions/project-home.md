# cluster-api-provider-lodia is an Apache-2.0 project with the Go modules go.lodia.io/cluster-api-provider-lodia and go.lodia.io/cluster-api-provider-lodia/api, the API group controlplane.lodia.io and the clusterctl name lodia

Decided 2026-10-26.

## Decision

- **Home:** `github.com/lodia-io/cluster-api-provider-lodia`.
  Issues and pull requests live there, and CI runs on GitHub Actions.
- **Go modules:** `go.lodia.io/cluster-api-provider-lodia`, a path of Lodia's own domain, as Lodia's `go.lodia.io/lodia`, for the manager.
  It imports `sigs.k8s.io/cluster-api/api` (its own module since v1.14) and `go.lodia.io/lodia/api` ([go.mod][lo-api-gomod]).
  The API is a module of its own in `api/`, `go.lodia.io/cluster-api-provider-lodia/api`: the package `v1alpha1` and the package `crds` with the two CRDs as `crds.FS`, as Lodia's `go.lodia.io/lodia/api` ([project-home][lo-project-home], [crds.go][lo-crds]) and Cluster API's `sigs.k8s.io/cluster-api/api`.
  A platform imports it without the manager's dependencies.
- **How the manager requires the API:** `require go.lodia.io/cluster-api-provider-lodia/api v0.0.0-00010101000000-000000000000` with `replace go.lodia.io/cluster-api-provider-lodia/api => ./api`, the form of Lodia's `go.mod` ([require][lo-gomod-require], [replace][lo-gomod-replace]) and of Cluster API's ([replace][cp-gomod-replace], [require][cp-gomod-require]).
  A replace binds only the main module ([replace](https://go.dev/ref/mod#go-mod-file-replace)), so a build of the tree takes the API next to it and needs no tag.
- **The pins agree:** `api/go.mod` and `go.mod` require the same `go.lodia.io/lodia/api` and the same `sigs.k8s.io/cluster-api/api`, and `TestTheAPIModulePinsWhatTheMainModuleDoes` fails when they differ.
- **API:** group `controlplane.lodia.io`, version `v1alpha1`, kinds `LodiaControlPlane` and `LodiaControlPlaneTemplate`; the CRDs are `lodiacontrolplanes.controlplane.lodia.io` and `lodiacontrolplanetemplates.controlplane.lodia.io`.
  The finalizer and the annotations are named under the group: `controlplane.lodia.io/lodiacontrolplane` and `controlplane.lodia.io/uid`.
- **clusterctl:** the provider is named `lodia`, so every manifest carries the label `cluster.x-k8s.io/provider: control-plane-lodia`, and the variables it defines start with `LODIA_`.
- **License:** Apache License 2.0, and contributions under the Developer Certificate of Origin (DCO sign-off), as Lodia's.
  Every Go file starts with Lodia's short Apache-2.0 header.
- What the provider shares with Lodia, the license, the DCO and the domain of the Go module, is Lodia's [project-home](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/project-home.md).

## Why

- The API module requires only what the types and their tests import, with no controller-runtime and no `sigs.k8s.io/cluster-api`, so a platform that imports the types takes none of the manager's other dependencies.
  Its floor for `k8s.io/*` is v0.37.2 at Cluster API v1.15.0-beta.0, which the types hold through `v1beta2.StatusVersion` ([go.mod][cp-api-gomod]); Lodia's floor of v0.35.0 cannot be kept in it.
- The pins agree because `api/v1alpha1/crd_test.go` compares the provider's CRDs with Lodia's at the API module's pin, while the e2e runs Lodia at the manager's.
- The subgroup `controlplane.lodia.io` keeps the provider's kinds apart from Lodia's `lodia.io`, and the contract names a control plane CRD `<plural>.<group>` ([definition][cp-crd], [CalculateCRDName][crd-name]) with `<env>ControlPlane` as the kind's convention.
- clusterctl labels the manifests of a provider with `cluster.x-k8s.io/provider: control-plane-<name>` for the name it is configured under ([label][cc-label]), and the contract's rules for the built-in list prefix the organization's name and drop the prefix where it would stutter, which gives `lodia` ([names][cc-names]).

## Rejected

- The group `controlplane.cluster.x-k8s.io`, which control plane providers under the Kubernetes SIGS org generally use ([contract][cp-group]) — the domain is ours.

## Consequences

- The repository carries `LICENSE`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md` and `MAINTAINERS.md`.
- `make lint`, `make test` and `make verify` run both modules, and `make verify` holds both tidy; `go test ./...` from the root no longer reaches `api/`.
- The API's tags are cut with the release's ([releases-through-clusterctl](releases-through-clusterctl.md)).
- `v0.1.0-alpha.1` holds `api/v1alpha1` in the manager's module, so a build that requires it together with the API module finds the package in two modules and fails with an ambiguous import ([ambiguous imports](https://go.dev/ref/mod#resolve-pkg-mod)): a platform requires the API module alone.
- With a `replace` in its `go.mod`, the manager cannot be built by `go install` or `go run` at a version ([cmd/go][go-replace]), as with Lodia and Cluster API; it ships as an image.
- A group other than Cluster API's needs the aggregated ClusterRole that gives Cluster API's core controllers access to the kinds ([group][cp-group]), which `config/rbac/aggregated_role.yaml` is.
- A name other than `lodia`, if clusterctl's built-in list asks for another, needs the label in `config/default/kustomization.yaml` changed with it ([`todo.md`](../../todo.md)).

## Code

- `go.mod` — the module path, the two imports, and the require and the replace of the API module; `api/go.mod` — the API module.
- `api/crds/crds.go` — `crds.FS`; `Makefile` — `generate`, which writes the CRDs into `api/crds` and copies them into `config/crd/bases` for kustomize ([the copy](../README.md#the-api-module)).
- `test/e2e/pin_test.go` — `TestTheAPIModulePinsWhatTheMainModuleDoes`.
- `api/v1alpha1/groupversion_info.go` — the group and the version; `api/v1alpha1/lodiacontrolplane_types.go` has the finalizer and the annotation names.
- `config/default/kustomization.yaml` — the provider label.
- `hack/boilerplate.go.txt` — the header of every Go file, which `make generate` puts on the generated ones too.
- `LICENSE`.

<!-- Cluster API v1.15.0-beta.0 -->
[cp-gomod-replace]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/go.mod#L5
[cp-gomod-require]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/go.mod#L57
[cp-api-gomod]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/api/go.mod#L6-L8
[cp-group]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L95-L141
[cp-crd]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/control-plane.md?plain=1#L178-L227
[crd-name]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/contract/contract.go#L28-L30
[cc-label]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L543-L564
[cc-names]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L34-L82

<!-- Lodia at 5737583 -->
[lo-api-gomod]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/go.mod#L1-L10
[lo-project-home]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/project-home.md#decision
[lo-crds]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/crds/crds.go#L10-L13
[lo-gomod-require]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/go.mod#L19
[lo-gomod-replace]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/go.mod#L167

<!-- Go 1.26.10 -->
[go-replace]: https://github.com/golang/go/blob/go1.26.10/src/cmd/go/internal/load/pkg.go#L3428-L3433
