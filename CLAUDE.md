# CLAUDE.md

cluster-api-provider-lodia is the Cluster API control plane provider for Lodia: a `LodiaControlPlane` per `Cluster` owns one Lodia `ControlPlane`, and worker nodes join through Cluster API's kubeadm bootstrap provider.
The fact that frames everything: **the provider builds nothing of a control plane**.
It writes one Lodia `ControlPlane` and two Secrets per `Cluster`, and reports Lodia's status in the form Cluster API's control plane contract reads.

## Where truth lives

- `docs/README.md` — what the provider is and does: the objects, the contract as built, a pass, the manager, installation, `clusterctl move`.
  A change to behaviour updates it in the same commit.
- `docs/decisions/` — indexed in `docs/README.md`.
  Read the relevant ones before changing the API, a condition, a grant or a security property; a decision is changed in its file, never worked around in code.
- `test/e2e/README.md` — the end-to-end suite: what it installs, its steps and its bounds.
- `CONTRIBUTING.md` — build, test, commits and cutting a release.
- `todo.md` — findings and work waiting for triage.
- Lodia's `docs/README.md` and `docs/decisions/` at the commit `LODIA_COMMIT` of the `Makefile`, which is also the pseudo-version of `go.lodia.io/lodia/api` in `go.mod`.
- The platform rules of Segla's handbook apply to how we work (https://gitlab.com/segla/handbook/-/blob/main/README.md): decisions in the format of `writing/decisions.md`, documentation layout from `writing/docs-layout.md`.

## Working from a task

A task's body is its work order (handbook `process/work-orders.md`).
Read **Authority** first; **Out of scope** is binding, findings outside it go to `todo.md`; a task is not Done until **Landing** is paid: the docs above, in the same change.

## Commands

- `make generate` — deepcopy functions, the CRDs in `config/crd/bases` and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result.
- `make verify` — the one check whose exit code says a change is done: generated files current, modules tidy, `config/default` rendered and checked, `metadata.yaml`, the release's render with no push, lint, unit and envtest tests.
  It needs `origin/main` fetched, and CI runs it on every pull request.
- `make e2e` — the suite in `test/e2e` on a kind cluster of its own; it refuses any other cluster (`docs/decisions/e2e-on-a-named-cluster.md`), and CI runs it.
- `make release-check TAG=vX.Y.Z` — refuses a tag before it is pushed.
- Cluster API's two modules move in one `go get`: `go get sigs.k8s.io/cluster-api/api@vX sigs.k8s.io/cluster-api@vX`.
  `sigs.k8s.io/cluster-api@vX` alone leaves `sigs.k8s.io/cluster-api/api` at the old version.

## Layout

- `config/crd/bases`, `config/rbac/role.yaml` and `api/v1alpha1/zz_generated.deepcopy.go` are generated and never edited; `make verify` fails when `make generate` changes them.
- `test/e2e` is behind the build tag `e2e`: `go test ./...` runs unit and envtest tests only.
- Two tests read the docs, so a change to them can fail a test: `api/v1alpha1/names_test.go` reads every tracked Markdown file for names of the API group that the API does not define, and `cmd/cluster-api-provider-lodia/main_test.go` reads the operator's fields in `docs/README.md`.

## Rules

- English everywhere in the repository, one sentence per line in Markdown; diagrams are mermaid inside Markdown.
- A claim about Cluster API, Lodia or Kubernetes carries a link to its source at a tag or a commit, Cluster API's at the pin in `go.mod`.
  No source, no claim.
- No admission webhooks: validation is CEL in the CRD schema (`docs/decisions/no-admission-webhooks.md`).
- Nothing in tests or tooling ever targets the current kube context: the e2e takes a named kubeconfig or refuses to run, and the rules for tests that drive clusterctl are in `.claude/rules/e2e.md`.
- Lodia's API is imported from `go.lodia.io/lodia/api`, never copied.
- Commits follow conventional commits (`feat:`, `fix:`, `docs:`, `chore:`), signed and signed off, with no generated attribution trailers.
