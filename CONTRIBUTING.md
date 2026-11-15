# Contributing to cluster-api-provider-lodia

[`docs/README.md`](docs/README.md) says what the provider does and [`docs/decisions/`](docs/decisions/) why; a pull request that changes behaviour updates them in the same change, and one that goes against a decision changes the decision's file first.
Taking part means following the [code of conduct](CODE_OF_CONDUCT.md).

## Build and test

You need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.13.2.
Every other tool runs through `go tool` or `go run`; `make e2e` also needs what [its README](test/e2e/README.md) lists.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `api/crds` with their copy in `config/crd/bases`, and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, both modules tidy, `config/default` rendered and checked for a non-empty default on every variable, one Namespace holding every namespaced object, the provider label on every object and a `manager` container as rendered, and for the roles that reach the manager's ServiceAccount on what clusterctl installs with the defaults and with explicit values, the render against the components of the release that [`test/pin`](test/pin/previous-release.yaml) names (a cluster-scoped object that clusterctl's upgrade would leave behind, a `roleRef` it would fail on, a CRD version it would stop serving; the first run downloads them into `work/pin`), `metadata.yaml` against the CRDs' contract label and the merge base with `origin/main` (fetch it first), the release's render with no push, lint and unit and envtest tests of both modules; the render has ko build both platforms and pull the base image from gcr.io, which takes minutes when the Go caches are cold |
| `make e2e` | the [end-to-end suite](test/e2e/README.md) on a kind cluster of its own |
| `make e2e-upgrade` | the [upgrade test](test/e2e/README.md#the-upgrade-test): the release that [`test/pin/previous-release.yaml`](test/pin/previous-release.yaml) names, installed through clusterctl and upgraded to the tree's build, on a kind cluster of its own |
| `make e2e-minor` | the [minor step test](test/e2e/README.md#the-minor-step-test): a Cluster at v1.36.4 with a worker that joined, its control plane stepped to v1.37.0 by Lodia while a second MachineDeployment at v1.37.0 waits for it, on a kind cluster of its own |
| `make release-components` | `config/default` with the manager's image built by ko for `PLATFORMS`, pushed to `IMAGE_REPOSITORY` unless `KO_PUSH=false` and named by digest, written to `out/control-plane-components.yaml`; the release and the e2e use it |
| `make release-image` | prints the image that `out/control-plane-components.yaml` names, after requiring it to be the only one and to read `IMAGE_REPOSITORY:IMAGE_TAG@sha256:<digest>` |
| `make release-check TAG=vX.Y.Z` | refuses a tag before it is pushed, as [the release steps](#release-a-version) say |

The repository holds two Go modules, the manager at the root and the API in `api/` ([project-home](docs/decisions/project-home.md)).
`make lint`, `make test` and `make verify` run both; `go test ./...` from the root does not reach `api/`, so run it there too.

CI runs `make verify`, `make e2e`, `make e2e-upgrade` and `make e2e-minor` on every pull request and every push to `main`.
The three e2e targets are the entries of one matrix job of `ci.yml`, whose checks are named `e2e`, `e2e-upgrade` and `e2e-minor`, as `main`'s ruleset requires ([how GitHub names a check](https://github.com/github/docs/blob/d59b2631959db46d71ca5f7748ec04311c6d0870/content/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/troubleshooting-rules.md?plain=1#L34-L40)).
A superseded run of a pull request is cancelled and a run of `main` never is, since `release-check` refuses a cancelled run.
Every job has a `timeout-minutes`, and every action is pinned to a commit with its version in a comment, as `release.yml`'s are; `test/build` holds each.
After `make verify`, its `verify` job runs `make verify ENVTEST_K8S_VERSION=not-a-version`, which `setup-envtest` rejects, and fails when that exits 0, since a `Makefile` that makes `make` ignore errors would pass `make verify`.
The tests of the `Makefile`, of the `go.mod` files and of that workflow are in [`test/build`](test/build), which `make test` runs; the end-to-end jobs run only [`test/e2e`](test/e2e), which keeps what reads the suite.
`make test`, which `make verify` runs, takes the variable from `setup-envtest` and stops when that fails, and `TestMakeVerifyRunsTheTestsOfTheModuleWithTheAssetsOfEnvtest` holds in the output of `make -n verify ENVTEST_K8S_VERSION=not-a-version` the line that does both for that version, so that a line that writes the version out is refused.
`make` ignores the failure of a recipe line that [starts with `-`](https://github.com/mirror/make/blob/4.3/doc/make.texi#L4426-L4428), of the recipes that a [`.IGNORE`](https://github.com/mirror/make/blob/4.3/doc/make.texi#L2994-L2999) names or, with no prerequisites, of all of them, and of [every recipe under `-i`](https://github.com/mirror/make/blob/4.3/doc/make.texi#L4447-L4450), which a [`MAKEFLAGS` in the makefile](https://github.com/mirror/make/blob/4.3/doc/make.texi#L4966-L4968) can set.
The output of `make -n verify` showed none of this on GNU make 3.81, and no test that `make test` runs can hold it, since a `make` that ignores errors ignores that test's failure too.
So the `verify` job of [`ci.yml`](.github/workflows/ci.yml) has a step after `make verify` that runs `make verify ENVTEST_K8S_VERSION=not-a-version`, a version that `setup-envtest` [rejects](https://github.com/kubernetes-sigs/controller-runtime/blob/tools/setup-envtest/v0.25.2/tools/setup-envtest/versions/parse.go#L49-L56) with [exit status 1](https://github.com/kubernetes-sigs/controller-runtime/blob/tools/setup-envtest/v0.25.2/tools/setup-envtest/main.go#L126-L130), and fails the job when that exits 0.
It runs `make verify` and not `make test` alone, so that a `-`, a `|| true` or a `-i` on the line of `verify` that runs the test rule, and a `.IGNORE: verify`, fail it too.
`TestTheVerifyJobRunsMakeVerifyWithAVersionSetupEnvtestRejectsAfterMakeVerify` holds that the step is there, follows `make verify`, runs the script that the test holds and has nothing but a name and a run, so that no `if`, `continue-on-error`, `shell` or `env` lets the job pass.
A `.IGNORE` for `lint` or another target before the test rule hides a failure that the step never meets, since those targets pass in it, so `TestTheMakefileHasNoIgnore` refuses every `.IGNORE` in the `Makefile`.

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
   Tag only a commit that has `make release-sign` and its steps in `release.yml`: a run is its tag's own commit's `release.yml` ([When a run fails](#when-a-run-fails)), so the release of an older commit has no signatures and no attestations; `make release-check` (step 4) refuses a tag at or below a published release of its line, which is the tag an older commit would get.
4. `make release-check TAG=vX.Y.Z` (and `COMMIT=<sha>` for a commit other than `HEAD`) fetches `origin/main` and the tags, and refuses:
   - a tag that is not written `vMAJOR.MINOR.PATCH[-PRERELEASE]`;
   - a tag whose minor has no series in `metadata.yaml` at the commit, and any tag of the repository whose minor has none there;
   - a tag at or below a published release of its line;
   - a commit that is not on `origin/main`;
   - a local tag `vX.Y.Z` or `api/vX.Y.Z` that is missing or is not at the commit;
   - a commit whose runs of `ci.yml` for the push to `main` are missing, unfinished or not green;
   - a commit whose pin of the upgrade e2e, [`test/pin/previous-release.yaml`](test/pin/previous-release.yaml), is not the newest published release below the tag, which it reads from GitHub's releases and not from the tags, since a failed run leaves a tag without a release.
   It reads GitHub with `GITHUB_TOKEN`, or with `gh auth token`.
   A refusal leaves both tags local: delete them, fix the cause and tag again.
5. Push both tags by name in one atomic push: `git push --atomic origin vX.Y.Z api/vX.Y.Z`.
   GitHub starts no workflow when more than three tags are pushed at once ([GitHub](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#push)), and such a tag gets no run, no release and no failed job.
   The workflow runs the same check again before it builds anything, and its checkout fetches every tag, so it finds `api/vX.Y.Z`.
6. The workflow pushes the image for both platforms, renders `control-plane-components.yaml` with it, pulls the image by its digest without credentials, signs it and both files, verifies the signatures, attests the image and both files, verifies the attestations ([Signing](#signing)), and then creates the release with `gh release create --verify-tag`, with generated notes, the two bundles and, for a pre-release tag, the pre-release flag.
   If the release job does not succeed, the `cleanup` job deletes the draft releases of the tag.
7. Once the release is published, move the pin of the upgrade e2e to it in a pull request, so that the next tag is upgrade-tested from this one:
   - download `control-plane-components.yaml` and `metadata.yaml` with their two bundles from the release, and run `cosign verify-blob` of both files first, with the tag's identity, as [Verifying a release](docs/README.md#verifying-a-release) says;
   - set `version`, `components` and `metadata` of [`test/pin/previous-release.yaml`](test/pin/previous-release.yaml) to the tag and to the sha256 of the two files, `shasum -a 256` of each, which the release's page also shows.
   `make verify` then downloads the new release's components into `work/pin`, and `make e2e-upgrade` installs the release and upgrades it to the tree.

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
