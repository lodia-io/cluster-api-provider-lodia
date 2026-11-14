# Contributing to cluster-api-provider-lodia

[`docs/README.md`](docs/README.md) says what the provider does and [`docs/decisions/`](docs/decisions/) why; a pull request that changes behaviour updates them in the same change, and one that goes against a decision changes the decision's file first.
Taking part means following the [code of conduct](CODE_OF_CONDUCT.md).

## Build and test

You need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.13.2.
Every other tool runs through `go tool` or `go run`; `make e2e` also needs what [its README](test/e2e/README.md) lists.

| command | does |
|---|---|
| `make generate` | deepcopy functions, the CRDs in `config/crd/bases` and the controller's ClusterRole in `config/rbac/role.yaml`; commit the result |
| `make verify` | the check that says a change is done: generated files current, modules tidy, `config/default` rendered and checked for a non-empty default on every variable, one Namespace holding every namespaced object, the provider label on every object and a `manager` container as rendered, and for the roles that reach the manager's ServiceAccount on what clusterctl installs with the defaults and with explicit values, `metadata.yaml` against the CRDs' contract label and the merge base with `origin/main` (fetch it first), the release's render with no push, lint, unit and envtest tests; the render has ko build both platforms and pull the base image from gcr.io, which takes minutes when the Go caches are cold |
| `make e2e` | the [end-to-end suite](test/e2e/README.md) on a kind cluster of its own |
| `make release-components` | `config/default` with the manager's image built by ko for `PLATFORMS`, pushed to `IMAGE_REPOSITORY` unless `KO_PUSH=false` and named by digest, written to `out/control-plane-components.yaml`; the release and the e2e use it |
| `make release-image` | prints the image that `out/control-plane-components.yaml` names, after requiring it to be the only one and to read `IMAGE_REPOSITORY:IMAGE_TAG@sha256:<digest>` |
| `make release-check TAG=vX.Y.Z` | refuses a tag before it is pushed, as [the release steps](#release-a-version) say |

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
What a release holds and why it is cut this way: [releases-through-clusterctl](docs/decisions/releases-through-clusterctl.md).

1. A tag that starts a new minor needs the minor's series in `metadata.yaml`, added in a pull request before the tag, with the contract that the CRDs carry.
   `make metadata`, which `make verify` runs, refuses a series that the merge base with `origin/main` has and the file lacks, a series whose contract changed, and a newest series whose contract is not the CRDs' contract label.
2. Wait for the CI of `main` to finish on the commit: a tag pushed before it ends is refused.
3. `make release-check TAG=vX.Y.Z` (and `COMMIT=<sha>` for a commit other than `HEAD`) fetches `origin/main` and the tags, and refuses:
   - a tag that is not written `vMAJOR.MINOR.PATCH[-PRERELEASE]`;
   - a tag whose minor has no series in `metadata.yaml` at the commit, and any tag of the repository whose minor has none there;
   - a commit that is not on `origin/main`;
   - a commit whose runs of `ci.yml` for the push to `main` are missing, unfinished or not green.
   It reads GitHub with `GITHUB_TOKEN`, or with `gh auth token`.
4. Tag the commit and push the one tag by name: `git tag -s -m vX.Y.Z vX.Y.Z <sha>`, then `git push origin vX.Y.Z`.
   GitHub starts no workflow when more than three tags are pushed at once ([GitHub](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#push)), and such a tag gets no run, no release and no failed job.
   The workflow runs the same check again before it builds anything.
5. The workflow pushes the image for both platforms, renders `control-plane-components.yaml` with it, pulls the image by its digest without credentials, and then creates the release with `gh release create --verify-tag`, with generated notes and, for a pre-release tag, the pre-release flag.
   If the release job does not succeed, the `cleanup` job deletes the draft releases of the tag.

Between the push of a tag and the publication of its release, `clusterctl upgrade plan` fails for every management cluster that has the provider: clusterctl takes the versions from proxy.golang.org, which lists a tag whether or not it has a release, and then asks for the files of the newest one ([versions](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_github.go#L104-L146), [proxy](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/goproxy/goproxy.go#L59-L143), [upgrade info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)).
The window is the run of the release job, which builds and publishes nothing before the tag.

### When a run fails

- A cause outside the commit, such as the package's visibility, the registry or a permission, is answered by re-running the failed job on the same tag.
  A re-run replays the tag's own commit, so it runs that commit's `release.yml`, `Makefile` and check.
- A cause inside the commit, in `release.yml`, the `Makefile` or the check, is fixed on `main`.
  Then delete the tag and tag the fixed commit again, or tag the next pre-release.
  Let the first run end before deleting its tag: `gh release create` names the tag, so a run still building publishes the assets of its own commit under a tag pushed again, and stays green.
- A tag that the workflow refuses or fails on anyway is answered at once by tagging the next pre-release of the same line, `v0.1.0-alpha.2` after `v0.1.0-alpha.1`, and never a stable release before `v0.1.0` is due.
  Delete the bad tag too, but nothing relies on that, since whether the proxy stops listing a deleted tag is not known.

### Signing

Keyless signing needs `id-token: write` in `release.yml` ([GitHub](https://docs.github.com/en/actions/concepts/security/openid-connect)); [`todo.md`](todo.md) holds the pull request that adds it.
