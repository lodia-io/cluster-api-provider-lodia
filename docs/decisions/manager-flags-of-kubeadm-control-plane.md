# The manager takes the flags of Cluster API's kubeadm control plane provider, under their names and with their defaults

Decided 2026-11-11.

## Decision

- The binary takes the flags of Cluster API's kubeadm control plane provider that apply to it ([InitFlags][kcp-flags]), under their names and with their defaults, but for the concurrency: `--lodiacontrolplane-concurrency` defaults to 10, where that provider's `--kubeadmcontrolplane-concurrency` defaults to [100][kcp-concurrency].
  The flag is the controller's `MaxConcurrentReconciles`.
- It does not define `--feature-gates`, `--webhook-port` or `--webhook-cert-dir`, which that provider defines because it has gates and webhooks.
- Logging goes through klog, as in that provider.
- Diagnostics are `util/flags`' metrics server on `--diagnostics-address`: HTTPS with authentication and authorization, which the `tokenreviews` and `subjectaccessreviews` rules of the ClusterRole are for, and with the pprof endpoints and `/debug/flags/v`; `--insecure-diagnostics` serves plain HTTP with none of them ([util/flags][capi-flags]).
- `--namespace` sets the cache's `DefaultNamespaces`.
- `--watch-filter` puts [`ResourceHasFilterLabel`][capi-filter] on the watches of the `LodiaControlPlane` and the `Cluster`.
  Lodia's `ControlPlane` and the Secrets carry no such label, so their map functions read the `Cluster` and enqueue only when it has the label ([`HasWatchLabel`][capi-haslabel]).
  The filter is not on the cache, and the label is not copied onto Lodia's `ControlPlane`.
- A pass has one minute, hard-coded.

## Why

- cluster-api-operator writes the fields of a provider's `spec.manager` as flags of the manager, so the flags have Cluster API's names, and a flag that the binary does not define makes it exit at start.
  `--namespace` is a MUST of the clusterctl contract, and `--watch-filter` of several instances of a provider ([components][cc-components], [multiple instances][cp-multi]).
- Every object the controller reads or writes is in the `LodiaControlPlane`'s namespace, Lodia's Secrets included, and nothing it reads is cluster-scoped, so `--namespace` holds for all of it.
  The shipped RBAC stays a ClusterRole, since clusterctl installs every provider watching all namespaces ([contract][cc-components]).
- A cache with the filter would hide Lodia's objects.
- Lodia's controllers run ten passes at once ([workers][lo-workers]), so one tenant's blocked write does not hold up another's.
- Cluster API's kubeadm control plane provider gives its passes [three minutes][kcp-timeout] because they connect to every etcd member; a pass of this provider has the [`ReconciliationTimeout`][cr-timeout] of one minute.

## Rejected

- A zap logger, as Lodia's operator has ([main.go](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/cmd/lodia/main.go#L32-L35)) — the operator writes `--v` for any verbosity but 1, and the diagnostics endpoint's `/debug/flags/v` sets klog's level, which a zap logger would not read, so zap would leave both broken ([util/flags][capi-flags]).
- The five `--zap-*` flags and `--metrics-bind-address` of the first binary, kept with a shim — there are no users.

## Consequences

- Several instances of the provider from the released YAML are not supported ([the reference](../README.md#the-manager)); `--namespace` and `--watch-filter` are there for the contract and for the operator.
- The fields of cluster-api-operator's `spec.manager` fall in five groups by what they do to the manager, which [the reference](../README.md#under-cluster-api-operator) lists and a test compares with the operator's customizer.

## Code

- `cmd/cluster-api-provider-lodia/main.go` — `InitFlags`.
- `cmd/cluster-api-provider-lodia/main_test.go` — `TestTheFieldsThatWorkParseAndReachTheOptionTheyName`, `TestTheFieldsWhoseFlagIsNotDefinedAreRefused`, `TestTheFlagsOfTheOldBinaryAreGone`, `TestTheFlagsDefaultToClusterAPIsValues`, `TestTheManifestsArgsParseAndItsPortsFollowTheDefaults`, `TestTheLoggingFlagsReachKlogAndControllerRuntime` and `TestTheDocsListTheFieldsInTheGroupsOfTheTable`.
- `internal/controller/scope_test.go` — `TestTheNamespaceKeepsTheManagerFromAnotherNamespacesLodiaControlPlanes`, `TestTheWatchFilterReconcilesOnlyTheObjectsCarryingItsValue` and `TestAPairThatDisagreesOnTheWatchLabelIsReconciledAtLeastOnce`.
- `internal/controller/failures_test.go` — `TestTheControllerRunsTheConfiguredWorkersAndBoundsAPass` and `TestOneTenantsBlockedWriteDoesNotHoldUpAnother`.

<!-- Cluster API v1.15.0-beta.0 -->
[cp-multi]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/core/support-multiple-instances.md?plain=1#L25-L26
[cc-components]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md?plain=1#L215-L259
[kcp-flags]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L128-L217
[kcp-concurrency]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L160-L161
[kcp-timeout]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L446-L447
[capi-flags]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/flags/manager.go#L95-L157
[capi-filter]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/predicates/generic_predicates.go#L161-L177
[capi-haslabel]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/labels/helpers.go#L71-L77

<!-- controller-runtime, kubeadm, Kubernetes -->
[cr-timeout]: https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.2/pkg/config/controller.go#L96-L98

<!-- Lodia at 5737583 -->
[lo-workers]: https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/internal/operator/operator.go#L40-L42
