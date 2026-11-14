# The kubeadm bootstrap provider may get a LodiaControlPlane, through a binding to its user that names it with two clusterctl variables

Decided 2026-11-10.
The binding is a workaround, and [`todo.md`](../../todo.md) holds what undoes it.

## Decision

- A ClusterRole grants `get` on `lodiacontrolplanes` and nothing else.
- A ClusterRoleBinding binds it to a subject of `kind: User` named `system:serviceaccount:${LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE:=capi-kubeadm-bootstrap-system}:${LODIA_KUBEADM_BOOTSTRAP_SA_NAME:=capi-kubeadm-bootstrap-manager}`, the name that a ServiceAccount authenticates as ([username][k8s-sa-username]).
- The binding ships in the components YAML with the two variables, and [the reference](../README.md#variables) documents them.
- The binding is the workaround, marked `@todo` at the site: it goes once the kubeadm bootstrap provider's ClusterRole aggregates a role for reading control planes.
  The grant stays, as a role that the kubeadm bootstrap provider aggregates.

## Why

- Cluster API's kubeadm bootstrap provider (CABPK) reads the object behind `Cluster.spec.controlPlaneRef` to render `.controlPlane.version` into files with `contentFormat: Template`, and v1.14.0 to v1.15.0-beta.0 read it on every worker join ([v1.14.0][cabpk-files-114], [v1.15.0-beta.0][cabpk-files]), while its ClusterRole grants that only for `kubeadmcontrolplanes` ([RBAC markers][cabpk-rbac]).
- [kubernetes-sigs/cluster-api#14470](https://github.com/kubernetes-sigs/cluster-api/pull/14470) stops the read on a join without templated files, so the grant stays for the files that still need it, and the binding goes when the kubeadm bootstrap provider aggregates a role.
- A User subject has no namespace for clusterctl to rewrite ([target namespace][cc-ns], [subjects][cc-subjects]).
- The ClusterRole of the kubeadm bootstrap provider aggregates no role ([kustomization](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/config/rbac/kustomization.yaml#L1-L6)), so the provider has to bind its own.

## Rejected

- A ServiceAccount subject — clusterctl sets the namespace of every subject that has one to the installed provider's own, and the subject would name an account that does not exist.

## Consequences

- clusterctl keeps no variable's value in the cluster, and processes the variables again at every `clusterctl upgrade`, so a value that is not given again falls back to its default, and the grant then names the default ServiceAccount ([the reference](../README.md#variables)).
- The binding's name does not start with the provider's namespace and a dash, so clusterctl does not delete it with the provider; [`todo.md`](../../todo.md) says what the release notes then say.

## Code

- `config/rbac/kubeadm_bootstrap_role.yaml` and `config/rbac/kubeadm_bootstrap_role_binding.yaml`, each with its `@todo`.
- `test/components/main_test.go` — `TestTheKubeadmBootstrapProvidersGrantIsGetOnLodiaControlPlanesAlone`, `TestEveryVariableHasANonEmptyDefault` and `TestTheRunsProcessWhatClusterctlInstalls`.
- `test/e2e/install_test.go` — `requireTheKubeadmBootstrapGrant` asks a SubjectAccessReview for the user and the groups of the installed kubeadm bootstrap provider's ServiceAccount, which must be allowed `get` and denied `update` on `lodiacontrolplanes`.
- `test/e2e/clusterctl_test.go` — `TestTheGrantOfTheInstalledKubeadmBootstrapProviderIsGetAlone` and `TestTheWorkersTemplateHoldsAFileRenderedFromTheControlPlanesVersion`: the worker's template holds a file with `contentFormat: Template`, so a join still needs the grant after the pin moves to v1.15.0.

<!-- Cluster API v1.15.0-beta.0 -->
[cabpk-rbac]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L79-L83
[cabpk-files]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L1023-L1027
[cabpk-files-114]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.0/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L1018-L1022
[cc-ns]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L242-L257
[cc-subjects]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L349-L361

<!-- controller-runtime, kubeadm, Kubernetes -->
[k8s-sa-username]: https://github.com/kubernetes/kubernetes/blob/v1.35.0/staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go#L53-L57
