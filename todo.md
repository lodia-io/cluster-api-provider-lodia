# To do

- Move `sigs.k8s.io/cluster-api` and `sigs.k8s.io/cluster-api/api` from `v1.15.0-beta.0` to v1.15.0 when it is released, planned for 2026-12-08 ([release plan](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/release/releases/release-1.15.md?plain=1#L24)), re-pin the links in `README.md`, `docs/design.md` and `test/e2e/README.md`, and move the releases in `test/e2e/install_test.go` as `test/e2e/README.md` says.
  Check the range of releases that read the control plane on every join, in `docs/design.md`, and the release `README.md` names, against the code at the new pin.
  Then run the e2e with the kubeadm bootstrap provider's grant removed from the render, and with `requireTheKubeadmBootstrapGrant` in `test/e2e/install_test.go` skipped, since that SubjectAccessReview fails first, and see the join fail on the file with `contentFormat: Template` in `test/e2e/testdata/workers.yaml`: at v1.15.0 a join without templated files no longer reads the control plane, and that file is what still needs the grant ([kubernetes-sigs/cluster-api#14470](https://github.com/kubernetes-sigs/cluster-api/pull/14470)).
  Name v1.15.0 as the requirement in `README.md`'s section "The Cluster API version" and in its `upgrade apply` command, and drop the versions from the `init` command and from the operator's `CoreProvider` and `BootstrapProvider`, since `latest` then installs v1.15.0 or a later release that carries [kubernetes-sigs/cluster-api#14389](https://github.com/kubernetes-sigs/cluster-api/pull/14389).
  Then make the repository public and tag `v0.1.0`, which ends the pre-releases `v0.1.0-alpha.N`; the item below has the order.
- Compare `.golangci.yml` with Lodia's copy at `LODIA_COMMIT` in `verify` and fail on any difference, once Lodia's file is the canonical one again.
  Lodia's copy still differs by an exclusion of gosec for `internal/testenv/`, which [Lodia's todo](https://github.com/lodia-io/lodia/blob/05c47ab109528a829134b4bc451a894c9aa6f60c/todo.md?plain=1#L228-L229) moves to `//nolint:gosec` at its sites, and by the first lines of its header, which the provider words for itself.
- Ship the kubeadm bootstrap provider's access to `LodiaControlPlane`s (`config/rbac/kubeadm_bootstrap_role.yaml`, `config/rbac/kubeadm_bootstrap_role_binding.yaml` and their lines in `config/rbac/kustomization.yaml`) as a role that the kubeadm bootstrap provider aggregates, without a binding of the provider's own.
  The access stays: [`docs/design.md`](docs/design.md#what-the-provider-writes) says why the provider needs it.
  The binding is the workaround: it grants `get` to the user `system:serviceaccount:${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE}:${LODIA_KUBEADM_BOOTSTRAP_SA_NAME}` because that provider's ClusterRole [aggregates no role](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/rbac/kustomization.yaml#L1-L6), and clusterctl [sets](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L361) the namespace of every namespaced subject to the provider's own, so a ServiceAccount subject would name an account that does not exist.
  Undo the binding and the two variables, in the manifests and in `README.md`, once the kubeadm bootstrap provider aggregates a role for reading control planes, and label the ClusterRole as that role selects.
  clusterctl deletes a cluster-scoped object of a provider only when its name starts with the provider's namespace and a dash ([delete](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/components.go#L133-L215)), which the binding's name, `cluster-api-provider-lodia-capi-kubeadm-bootstrap-lodia-controlplane`, does not: say in the release notes that the binding stays in every management cluster that clusterctl upgrades, and is to be deleted by hand.
  [kubernetes-sigs/cluster-api#14470](https://github.com/kubernetes-sigs/cluster-api/pull/14470) stops the read on a join without templated files; once releases carry it, restate the range in `docs/design.md`, and the release `README.md` names, for each release line, naming the first release of each that carries it.
- Make the repository public in lodia-io/terraform once the pin above has moved to v1.15.0: the repository goes public with the first stable release, `v0.1.0`, and the releases before it are pre-releases of a private repository.
  The steps, in this order:
  1. Confirm by hand that every semver tag has a published release, in a clone of the repository: from the first public day the Go proxy lists a tag whether or not it has a release, and when the newest listed tag has none `clusterctl upgrade plan` fails for every management cluster that has the provider ([README](README.md#release-a-version)).
     ```sh
     set -eo pipefail
     tags="$(git ls-remote --tags --refs origin | sed 's|.*refs/tags/||' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+' | sort)"
     releases="$(gh release list --exclude-drafts --limit 1000 --json tagName --jq '.[].tagName' | sort)"
     comm -23 <(echo "$tags") <(echo "$releases")
     ```
     It prints each tag without a published release, and nothing when every tag has one; a failed `git` or `gh` stops it, since an empty list would read as a pass.
     Give each tag it prints a release by re-running its run, or delete the tag, before the next step.
  2. The change in lodia-io/terraform (`repositories.tf`) that makes the repository public.
  3. The pull request of the signing, below.
  4. The tag `v0.1.0`.
- Sign the image and both release files keyless with cosign: `id-token: write` in `release.yml`, `cosign sign` of the image by digest, `cosign sign-blob --bundle` of `control-plane-components.yaml` and `metadata.yaml` with the bundles as release assets, and the verification in `README.md`.
  Wait for the change that makes the repository public, the second step of the item above: keyless signing writes the repository's name, the workflow's path, the commit and the repository's visibility into the public transparency log for good ([certificate](https://github.com/sigstore/fulcio/blob/v1.10.0/pkg/identity/github/principal.go#L191-L232), [visibility](https://github.com/sigstore/fulcio/blob/v1.10.0/docs/oid-info.md#L181-L183), [the log](https://github.com/sigstore/cosign/blob/v3.1.4/cmd/cosign/cli/sign/privacy/privacy.go#L21-L28)).
  The pull request lands after that change and before the first stable tag.
- Add an e2e that installs the previous release with clusterctl, creates a Cluster and upgrades the provider to the tree's build with `clusterctl upgrade apply --control-plane lodia:vX`, once a second release exists.
- Ask for the provider's place in clusterctl's built-in list ([the contract's rules for names](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L34-L82)), once the repository is public.
  The issue is the owner's to open, and the name the list takes is settled there; installations under `lodia` keep working through their `providers:` entry, and `clusterctl delete` and `init` align them with the list.
  A name other than `lodia` also needs the label in `config/default/kustomization.yaml` changed.
  Then the `providers:` entry and the operator's `fetchConfig.url` in `README.md` go.
- Publish `control-plane-components.yaml` and `metadata.yaml` as an OCI artifact for the operator's `fetchConfig.oci` once clusterctl reads OCI ([kubernetes-sigs/cluster-api#13729](https://github.com/kubernetes-sigs/cluster-api/issues/13729), open) or a user needs it.
  Operator users take `fetchConfig.url` to the GitHub release until then.
- Add an e2e that installs the provider through cluster-api-operator, the one test of `spec.manager` as users meet it, when a user reports an install through the operator or when the operator releases a customizer that differs from the copy of v0.30.0's in `cmd/cluster-api-provider-lodia/main_test.go`.
  That test holds the flag names meanwhile.
