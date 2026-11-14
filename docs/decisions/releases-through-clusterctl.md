# A release is a GitHub release that clusterctl and cluster-api-operator read, with an image on ghcr.io named by digest, cut from a signed tag by hand

Decided 2026-11-11.
Every release but `v0.1.0-alpha.1` is signed and attested.

## Decision

- A release is a [GitHub release](https://github.com/lodia-io/cluster-api-provider-lodia/releases) of a semver tag, with the two files that clusterctl and cluster-api-operator read, `control-plane-components.yaml` and `metadata.yaml` ([the contract](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L84-L102)).
- The manager's image is `ghcr.io/lodia-io/cluster-api-provider-lodia`, for linux/amd64 and linux/arm64, built by ko, in a public package: pulling it takes no credentials.
  `control-plane-components.yaml` names it `<repository>:<tag>@sha256:<digest>`, which pulls by digest.
- `control-plane-components.yaml` is `config/default` rendered by kustomize, and there is no Helm chart.
- Tags are cut by hand, signed and annotated, from a commit of `main` whose runs of `ci.yml` are green, and pushed after `make release-check`.
  `release.yml` runs on the push of a tag and checks it again before it builds anything ([the steps](../../CONTRIBUTING.md#release-a-version)).
- Every release tags the API module at the same commit and with the same version, `api/vX.Y.Z` beside `vX.Y.Z`, the form Go reads for a module in a subdirectory ([version tags](https://go.dev/ref/mod#vcs-version)), as Lodia's ([releases-and-support](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/releases-and-support.md#decision)).
  `make release-check` refuses a local `vX.Y.Z` or `api/vX.Y.Z` that is missing or at another commit, and both tags go in one atomic push.
- A pushed tag is never moved or deleted, the release's or the API's: a cause in the commit is answered by the next version of the line, the next pre-release while it has only pre-releases and the next patch once it has a stable release.
- `metadata.yaml` is committed and append-only: a minor's series is added in a pull request before its first tag, with the contract that the CRDs carry, and a series is never dropped.
- The releases are pre-releases, `v0.1.0-alpha.1` and on, until Cluster API v1.15.0 is out and the provider's pin has moved to it, and then `v0.1.0`.
- `release.yml` signs keyless, from the OIDC identity of its run at the tag, the image by digest, the index and each platform's image, and `control-plane-components.yaml` and `metadata.yaml`; the bundles `<file>.sigstore.json` are assets of the release.
  It also attests the build provenance of the image and of both files with GitHub artifact attestations.
  Right after it signs it runs what a user runs, `cosign verify` of the index and of each image the index lists and `cosign verify-blob` of both files, and right after it attests, `gh attestation verify` of the image and of both files, each pinned to the tag of the run, so that a signature or an attestation that a user's command would refuse stops the release before it is published ([verifying a release](../README.md#verifying-a-release)).
  `v0.1.0-alpha.1` is the one release without them.
- While Lodia has no release, the requirement on Lodia is a commit: the `LODIA_COMMIT` of the `Makefile` that a release is built and tested against ([installation](../README.md#installation)).
- The repository is public.

## Why

- clusterctl and the operator read the two files, and a digest in the manifest makes an `images:` override of the repository keep the pin ([the reference](../README.md#with-clusterctl)).
- `clusterctl upgrade plan` reads the newest release's `metadata.yaml` for every version of the provider that a management cluster may run, and fails for the whole management cluster when a series is missing ([upgrade info](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader_info.go#L51-L123)), so a series is never dropped.
- A refused tag must not reach the remote, and the release job builds and publishes nothing that it could have done before the tag, so that the window in which a tag is listed without its release stays the run of one job ([the window](../../CONTRIBUTING.md#release-a-version)).
- The repository is public for anonymous installs, the Go proxy and, later, a place in clusterctl's built-in list ([`todo.md`](../../todo.md)).
- Signatures are keyless and the releases carry GitHub artifact attestations, from the release workflow's OIDC identity, as Lodia's ([releases-and-support](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/releases-and-support.md)).
- The provider builds on Cluster API v1.15.0-beta.0 until v1.15.0 is released, and on a Lodia that has no release.
- The types change only with the provider, so one version answers which release serves which API.
- The Go proxy keeps the first content it served for a version ([the proxy's FAQ](https://proxy.golang.org/)) and the checksum database its hash, so a moved `api/` tag fails every fetch that is not cached ([checksum database](https://go.dev/ref/mod#checksum-database)).
  A deleted tag is no better: a version tag "should not be deleted or changed to a different revision", and its content may remain available on module proxies after it is deleted ([version tags](https://go.dev/ref/mod#vcs-version)).
- An `api/` tag starts no run, since `release.yml` runs on tags `v*`, and `release-check` skips it, as it skips any tag that `version.ParseSemantic` refuses, so the API's tags need no series in `metadata.yaml`.
  clusterctl lists the provider's versions from the Go proxy, which for the provider's own module lists no `api/` tag ([list](https://github.com/golang/go/blob/go1.26.10/src/cmd/go/internal/modfetch/coderepo.go#L163-L197), [clusterctl](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/goproxy/goproxy.go#L123-L131)).

## Rejected

- A Helm chart, as Lodia's ([releases-and-support](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/releases-and-support.md)) — what clusterctl reads is a components YAML, which Cluster API's kubeadm bootstrap provider renders with kustomize ([contract][cc-components], [kustomization](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/default/kustomization.yaml#L1-L4)).
- release-please, as Lodia's ([releases-and-support](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/releases-and-support.md)) — a minor's series in `metadata.yaml` is a hand edit either way.
- A private repository with a public package until the first stable release — clusterctl would need a token and the operator a `github-token` to read a release, for every `clusterctl upgrade` of any management cluster that has the provider.

## Consequences

- Operator users take `fetchConfig.url` to the GitHub release: an OCI artifact for `fetchConfig.oci` waits until clusterctl reads OCI ([`todo.md`](../../todo.md)).

## Code

- `Makefile` — `release-components`, `release-image`, `release-login`, `release-sign`, `release-verify-signatures`, `release-verify-attestations`, `release-check` and `metadata`; `make verify` runs the release's render without a push, `release-image` and `metadata`.
- `.github/workflows/release.yml` — the release and the cleanup job; `.ko.yaml` — the base image.
- `metadata.yaml` — the series; `hack/releasecheck` — the tag check, which includes both tags at the commit, and the metadata check.
- `hack/releasecheck/check_test.go` — `TestTheTagCheckRefuses` and `TestTheMetadataCheck`; `hack/releasecheck/release_test.go` — `TestTheReleaseWorkflowStartsOnEveryTagAndCleansUpUnlessTheReleaseSucceeds`, `TestTheReleaseWorkflowGrantsOnlyWhatItUses`, `TestTheReleaseWorkflowPullsTheImageThatTheMakefileChecks`, `TestTheReleaseWorkflowPinsEveryActionToACommitWithItsVersion`, `TestTheMakefileSignsAndVerifiesTheImageAndTheFilesOfTheRelease` and `TestVerifyChecksTheImageItRenders`.

<!-- Cluster API v1.15.0-beta.0 -->
[cc-components]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L215-L259
