# To do

- Move `sigs.k8s.io/cluster-api` and `sigs.k8s.io/cluster-api/api` from `v1.15.0-beta.0` to v1.15.0 when it is released, planned for 2026-12-08 ([release plan](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/release/releases/release-1.15.md?plain=1#L24)), and re-pin the links in `README.md` and `docs/design.md`.
- With the release artifacts: the manager's `--namespace` flag, a MUST for clusterctl, and `--watch-filter`, a MUST for several instances of a provider.
