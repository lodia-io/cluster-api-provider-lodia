# Contributing to cluster-api-provider-lodia

[`docs/README.md`](docs/README.md) says what the provider does and [`docs/decisions/`](docs/decisions/) why; a pull request that changes behaviour updates them in the same change, and one that goes against a decision changes the decision's file first.
Taking part means following the [code of conduct](CODE_OF_CONDUCT.md).

## Build and test

You need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.13.2.
Every other tool runs through `go tool` or `go run`; `make e2e` also needs what [its README](test/e2e/README.md) lists.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `api/crds` with their copy in `config/crd/bases`, and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, both modules tidy, `config/default` rendered and checked for a non-empty default on every variable, one Namespace holding every namespaced object, the provider label on every object and a `manager` container as rendered, and for the roles that reach the manager's ServiceAccount on what clusterctl installs with the defaults and with explicit values, `metadata.yaml` against the CRDs' contract label and the merge base with `origin/main` (fetch it first), the release's render with no push, lint and unit and envtest tests of both modules; the render has ko build both platforms and pull the base image from gcr.io, which takes minutes when the Go caches are cold |
| `make e2e` | the [end-to-end suite](test/e2e/README.md) on a kind cluster of its own |
| `make release-components` | `config/default` with the manager's image built by ko for `PLATFORMS`, pushed to `IMAGE_REPOSITORY` unless `KO_PUSH=false` and named by digest, written to `out/control-plane-components.yaml`; the release and the e2e use it |
| `make release-image` | prints the image that `out/control-plane-components.yaml` names, after requiring it to be the only one and to read `IMAGE_REPOSITORY:IMAGE_TAG@sha256:<digest>` |
| `make release-check TAG=vX.Y.Z` | refuses a tag before it is pushed, as [the release steps](#release-a-version) say |

The repository holds two Go modules, the manager at the root and the API in `api/` ([project-home](docs/decisions/project-home.md)).
`make lint`, `make test` and `make verify` run both; `go test ./...` from the root does not reach `api/`, so run it there too.

CI runs `make verify` and `make e2e` on every pull request and every push to `main`.

The envtest suite starts its own API server and etcd; nothing in it reads a kubeconfig.

## Docs

- Each fact once, in its home, and a link to it elsewhere: what the provider does and how it is installed in [`docs/README.md`](docs/README.md), why in [`docs/decisions/`](docs/decisions/), the end-to-end suite in [`test/e2e/README.md`](test/e2e/README.md).
  No history, and no prose that restates the code.
- Every claim about Cluster API, Lodia or Kubernetes carries a source at a tag or a commit, Cluster API's at the pin in `go.mod`.
- Markdown uses one sentence per line.

## Commits

- [Conventional commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `chore:`.
- Every commit is signed off: its `Signed-off-by` line certifies the [Developer Certificate of Origin](https://developercertificate.org/).
  `git commit -s` adds the line; the DCO check on a pull request refuses a commit without one.

## Release a version

A tag `vMAJOR.MINOR.PATCH[-PRERELEASE]` is a release: pushing it runs [`release.yml`](.github/workflows/release.yml), which checks the tag, pushes the image, renders `control-plane-components.yaml` and publishes the GitHub release with it and `metadata.yaml`.
A tag with a pre-release part makes a pre-release.
The API module is tagged at the same commit with the same version, `api/vMAJOR.MINOR.PATCH[-PRERELEASE]`, which starts no run.
What a release holds and why it is cut this way: [releases-through-clusterctl](docs/decisions/releases-through-clusterctl.md).

1. A tag that starts a new minor needs the minor's series in `metadata.yaml`, added in a pull request before the tag, with the contract that the CRDs carry.
   `make metadata`, which `make verify` runs, refuses a series that the merge base with `origin/main` has and the file lacks, a series whose contract changed, and a newest series whose contract is not the CRDs' contract label.
2. Wait for the CI of `main` to finish on the commit: a tag pushed before it ends is refused.
3. Tag the commit with both tags, signed and annotated: `git tag -s -m vX.Y.Z vX.Y.Z <sha>` and `git tag -s -m api/vX.Y.Z api/vX.Y.Z <sha>`.
   Tag only a commit that has `make release-sign` and its steps in `release.yml`: a run is its tag's own commit's `release.yml` ([When a run fails](#when-a-run-fails)), so the release of an older commit has no signatures and no attestations.
4. `make release-check TAG=vX.Y.Z` (and `COMMIT=<sha>` for a commit other than `HEAD`) fetches `origin/main` and the tags, and refuses:
   - a tag that is not written `vMAJOR.MINOR.PATCH[-PRERELEASE]`;
   - a tag whose minor has no series in `metadata.yaml` at the commit, and any tag of the repository whose minor has none there;
   - a commit that is not on `origin/main`;
   - a local tag `vX.Y.Z` or `api/vX.Y.Z` that is missing or is not at the commit;
   - a commit whose runs of `ci.yml` for the push to `main` are missing, unfinished or not green.
   It reads GitHub with `GITHUB_TOKEN`, or with `gh auth token`.
   A refusal leaves both tags local: delete them, fix the cause and tag again.
5. Push both tags by name in one atomic push: `git push --atomic origin vX.Y.Z api/vX.Y.Z`.
   GitHub starts no workflow when more than three tags are pushed at once ([GitHub](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#push)), and such a tag gets no run, no release and no failed job.
   The workflow runs the same check again before it builds anything, and its checkout fetches every tag, so it finds `api/vX.Y.Z`.
6. The workflow pushes the image for both platforms, renders `control-plane-components.yaml` with it, pulls the image by its digest without credentials, signs it and both files, verifies the signatures, attests the image and both files, verifies the attestations ([Signing](#signing)), and then creates the release with `gh release create --verify-tag`, with generated notes, the two bundles and, for a pre-release tag, the pre-release flag.
   If the release job does not succeed, the `cleanup` job deletes the draft releases of the tag.

Between the push of a tag and the publication of its release, `clusterctl upgrade plan` fails for every management cluster that has the provider: clusterctl takes the versions from proxy.golang.org, which lists a tag whether or not it has a release, and then asks for the files of the newest one ([versions](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_github.go#L104-L146), [proxy](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/goproxy/goproxy.go#L59-L143), [upgrade info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)).
The window is the run of the release job, which builds and publishes nothing before the tag.
A re-run on the same tag keeps the window open until it publishes the release.

### When a run fails

- A cause outside the commit, such as the registry or a permission, is answered by re-running the failed job on the same tag.
  A re-run replays the tag's own commit, so it runs that commit's `release.yml`, `Makefile` and check.
- A cause inside the commit, in `release.yml`, the `Makefile` or the check, is fixed on `main`.
  Then tag the next version of the line with both tags: the next pre-release while the line has only pre-releases, the next patch once it has a stable release.
- A tag that the workflow refuses or fails on anyway is answered at once the same way, `v0.1.0-alpha.2` after `v0.1.0-alpha.1`, and never a stable release before `v0.1.0` is due.
- No pushed tag is deleted or moved, the release's or the API's ([why](docs/decisions/releases-through-clusterctl.md#why)).

### Signing

`release.yml` signs keyless, with the OIDC token of its run ([GitHub](https://docs.github.com/en/actions/concepts/security/openid-connect)), and attests build provenance, so its release job has `id-token: write` and `attestations: write`, and no other job has either.
`make release-sign` signs with cosign:
- the image by digest, the index and each platform's image, with `--recursive`;
- `control-plane-components.yaml` and `metadata.yaml`, with a bundle `<file>.sigstore.json` each, which the release carries as assets.

`make release-verify-signatures` then runs what a user runs ([verifying a release](docs/README.md#verifying-a-release)): `cosign verify` of the index and of each image it lists, and `cosign verify-blob` of both files, each pinned to the identity of `release.yml` at the run's tag.
Only then `actions/attest-build-provenance` attests the image, pushing the attestation to the registry, and both files, because the attestation that the action pushes is a Sigstore bundle in the registry ([push](https://github.com/actions/attest/blob/v4.2.1/src/attest.ts#L45-L65)), and once the index has a bundle `cosign verify` of it takes the bundles as the signatures ([verify](https://github.com/sigstore/cosign/blob/v3.1.4/cmd/cosign/cli/verify/verify.go#L160-L166)), so a check made after the attestation would not show a missing signature.
`make release-verify-attestations` runs `gh attestation verify` of the image and of both files, pinned to the repository, to `release.yml` and to the ref of the run's tag, which makes each attestation the tag's own.
A signature or an attestation that a user's command would refuse fails the job and nothing is published.
