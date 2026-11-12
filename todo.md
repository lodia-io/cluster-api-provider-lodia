# To do

- Move `sigs.k8s.io/cluster-api` and `sigs.k8s.io/cluster-api/api` from `v1.15.0-beta.0` to v1.15.0 when it is released, planned for 2026-12-08 ([release plan](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/release/releases/release-1.15.md?plain=1#L24)), re-pin the links in `README.md`, `docs/design.md` and `test/e2e/README.md`, and move the releases in `test/e2e/install_test.go` as `test/e2e/README.md` says.
  Check the range of releases that read the control plane on every join, stated in `README.md` and `docs/design.md`, against the code at the new pin.
- With the release artifacts: the manager's `--namespace` flag, a MUST for clusterctl, and `--watch-filter`, a MUST for several instances of a provider.
- Compare `.golangci.yml` with Lodia's copy at `LODIA_COMMIT` in `verify` and fail on any difference, once Lodia's file is the canonical one again.
  Lodia's copy still differs by an exclusion of gosec for `internal/testenv/`, which [Lodia's todo](https://github.com/lodia-io/lodia/blob/05c47ab109528a829134b4bc451a894c9aa6f60c/todo.md?plain=1#L228-L229) moves to `//nolint:gosec` at its sites, and by the first lines of its header, which the provider words for itself.
- Publish the manager's image and the manifests of `config/default` with a release.
- Ship the kubeadm bootstrap provider's access to `LodiaControlPlane`s (`config/rbac/kubeadm_bootstrap_role.yaml`, `config/rbac/kubeadm_bootstrap_role_binding.yaml` and their lines in `config/rbac/kustomization.yaml`) outside the components YAML of the release artifacts.
  The access stays: [`docs/design.md`](docs/design.md#what-the-provider-writes) says why the provider needs it.
  The binding is the workaround: it names the ServiceAccount of the kubeadm bootstrap provider in `capi-kubeadm-bootstrap-system` because that provider's ClusterRole [aggregates no role](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/rbac/kustomization.yaml#L1-L6), and clusterctl [sets](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L361) the namespace of every namespaced subject to the provider's own, so a clusterctl install binds a ServiceAccount that does not exist.
  Undo the binding once the kubeadm bootstrap provider aggregates a role for reading control planes, and label the ClusterRole as that role selects.
  [kubernetes-sigs/cluster-api#14470](https://github.com/kubernetes-sigs/cluster-api/pull/14470) stops the read on a join without templated files; once releases carry it, restate the range in `README.md` and `docs/design.md` for each release line, naming the first release of each that carries it.
- Close four gaps in the e2e's unit tests:
  - `TestTheDumpEndsAHeldListOfPodsOfTheLogsAtTheRequestTimeout` failed 5 of 360 runs beside 250 busy loops and 60 fork storms on a 16-core Mac, with 200 ms requests and a 1 s margin; give it a longer bound and longer requests, and count the lists from the dump's error;
  - no test requires the managers' logs, one file per namespace with the content served;
  - no test holds the tenant's part to the dump's bound, with a `docker` that hangs and a tenant that does not answer;
  - `TestTheStepsCallDockerOnlyThroughItsBound` sees only the identifier `docker` inside function declarations, so a call through a function value or a raw `exec` passes it; bound every call in `docker` itself and drop the scan.
