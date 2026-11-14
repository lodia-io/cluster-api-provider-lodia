# The e2e suite runs only against its own kind cluster, installs the provider through clusterctl as users do, and bounds every wait

Decided 2026-11-09.

## Decision

- The suite runs only against a kind cluster whose kubeconfig is named in `E2E_KUBECONFIG`, and never reads the current kube context.
  `guard.go` wants a single context whose name starts with `kind-capl-e2e` and whose server is a loopback address, and refuses to start while a `HELM_KUBE*` variable is set.
  `make e2e` creates the kind cluster `capl-e2e`, or the one `E2E_CLUSTER` names, and deletes it afterwards; `make e2e-cluster` refuses a name that the guard or Lodia would refuse, before anything is created.
- The prefix is not Lodia's `lodia-e2e`, so this guard refuses Lodia's own e2e cluster, and Lodia's guard refuses this one.
- Cluster API, its kubeadm bootstrap provider, Cluster API's Docker provider and this provider are installed through clusterctl's `Init`, on a local repository the suite lays out, with the provider's components YAML rendered by the target that renders the release's.
  Lodia and etcd-druid are installed from Lodia's chart at the `LODIA_COMMIT` of the `Makefile`.
- A worker joins through Cluster API's kubeadm bootstrap provider with the bootstrap objects that Lodia's Tenant controller writes, and a pod runs on it, read through the `<cluster>-kubeconfig` that the provider wrote.
- A test that drives clusterctl or loads a kubeconfig first points `KUBECONFIG` at a kubeconfig of a cluster that never answers, and fails if that cluster is asked anything.
- Every wait has a bound in the suite's model, the bounds add up to less than `go test`'s `-timeout`, and `-timeout` to less than the job's `timeout-minutes`.
  A wait ends through the suite's own end of its window, by cancelling its context, never by a context deadline.
- CI runs `make e2e` as the job `e2e`, on every pull request and every push to `main`.
- What the provider shares with Lodia's guard is Lodia's [decision of the same name](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/e2e-on-a-named-cluster.md).

## Why

- Lodia's own e2e cluster is `kind-lodia-e2e-local`, in which the suite would upgrade Lodia, so the two guards refuse each other's clusters.
- The goal is Lodia's roadmap, phase 3: a `Cluster` of this provider reaches `ControlPlaneInitialized`, and a `MachineDeployment` of Cluster API's Docker machines joins it through the kubeadm bootstrap provider, proved on real objects: the Node `Ready` and a pod running on it.
- Installing the YAMLs one object at a time leaves no clusterctl processing, label or inventory to test, and clusterctl's owner graph would see no `Cluster`.
  Through clusterctl, the path users take is the one tested, and the release's own render is what runs.
- On 2026-11-13 a mutant of the suite's clusterctl factory made clusterctl fall back to its own proxy, which loads the current context's kubeconfig, and a test run reached a live cluster.
  The trap turns such a fallback into a failing test.
- A step's own bound has to fail before `go test` gives up, since only the step's failure writes the artifacts.
- A request that starts under a deadline is refused by client-go's rate limiter at once, with an error that is no context's, when its token is due after the deadline, which a tick at the end of a window brings about ([`test/e2e/README.md`](../../test/e2e/README.md)).

## Rejected

- Cluster API's `test/framework` with Ginkgo — its top-level helpers assume a `KubeadmControlPlane`, it forces Ginkgo, and it costs 42 new modules against 8 for `sigs.k8s.io/e2e-framework`, which Lodia's suite uses too ([Lodia's decision](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/docs/decisions/e2e-on-a-named-cluster.md)).

## Consequences

- The suite needs Docker and helm, and runs where the Docker network `kind` is routable, as on GitHub's Linux runners.
  Docker Desktop does not route it, so the suite does not run on macOS.
- A new bound joins the model in `TestTheBoundsFitUnderTheTimeouts`, and a new wait is checked by `TestEveryWaitOfTheJoinTestIsBoundedByAFieldOfTheBoundsThatNoOtherWaitTakes`.

## Code

- `test/e2e/guard.go` — `RequireNamedCluster`; `test/e2e/guard_test.go` — `TestRequireNamedCluster`, `TestMakeAndTheGuardAgreeOnTheClusterName` and `TestMakeAcceptsOnlyNamesWhoseClusterLodiaAccepts`.
- `Makefile` — `e2e`, `e2e-cluster` and `e2e-lodia`; `.github/workflows/ci.yml` — the job `e2e`.
- `test/e2e/join_test.go` — `TestAWorkerJoinsAClusterOfLodiasControlPlane`; `test/e2e/install_test.go` and `test/e2e/clusterctl_test.go` — the install; `test/e2e/README.md` — the steps and the failure modes.
- `test/clusterctlproxy/trap.go` — `TrapTheDefaultKubeconfig`; `test/clusterctlproxy/proxy_test.go` — `TestNoCallReadsAKubeconfigOrTheCurrentContext` and `TestEveryClusterctlClientIsBuiltThroughTheFactory`.
- `test/e2e/bounds_test.go` — `TestTheBoundsFitUnderTheTimeouts`; `test/e2e/stable_test.go` — `endAfter`.
