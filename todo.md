# To do

- Move `sigs.k8s.io/cluster-api` and `sigs.k8s.io/cluster-api/api` from `v1.15.0-beta.0` to v1.15.0 when it is released, planned for 2026-12-08 ([release plan](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/release/releases/release-1.15.md?plain=1#L24)), and re-pin the links in `README.md` and `docs/design.md`.
- With the release artifacts: the manager's `--namespace` flag, a MUST for clusterctl, and `--watch-filter`, a MUST for several instances of a provider.
- Publish the manager's image and the manifests of `config/default` with a release.
- Ship the kubeadm bootstrap provider's access to `LodiaControlPlane`s (`config/rbac/kubeadm_bootstrap_role.yaml`, `config/rbac/kubeadm_bootstrap_role_binding.yaml` and their lines in `config/rbac/kustomization.yaml`) outside the components YAML of the release artifacts.
  The access stays: [`docs/design.md`](docs/design.md#what-the-provider-writes) says why the provider needs it.
  The binding is the workaround: it names the ServiceAccount of the kubeadm bootstrap provider in `capi-kubeadm-bootstrap-system` because that provider's ClusterRole [aggregates no role](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/rbac/kustomization.yaml#L1-L6), and clusterctl [sets](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L361) the namespace of every namespaced subject to the provider's own, so a clusterctl install binds a ServiceAccount that does not exist.
  Undo the binding once the kubeadm bootstrap provider aggregates a role for reading control planes, and label the ClusterRole as that role selects.
