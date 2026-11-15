# The e2e suite runs only against its own kind cluster, installs the provider through clusterctl as users do, and bounds every wait

Decided 2026-11-09; the second job, `e2e-upgrade`, and the third, `e2e-minor`, added 2026-11-15; the waits' end by cancelling a context, with the client's rate limiter kept, replaced 2026-11-15 by a deadline and no limiter.

## Decision

- The suite runs only against a kind cluster whose kubeconfig is named in `E2E_KUBECONFIG`, and never reads the current kube context.
  `guard.go` wants a single context whose name starts with `kind-capl-e2e` and whose server is a loopback address, and the suite refuses to start while a `HELM_KUBE*` variable is set (`refuseHelmKubeEnv`).
  `make e2e` creates the kind cluster `capl-e2e`, or the one `E2E_CLUSTER` names, and deletes it afterwards; `make e2e-cluster` refuses a name that the guard or Lodia would refuse, before anything is created.
- The prefix is not Lodia's `lodia-e2e`, so this guard refuses Lodia's own e2e cluster, and Lodia's guard refuses this one.
- Cluster API, its kubeadm bootstrap provider, Cluster API's Docker provider and this provider are installed through clusterctl's `Init`, on a local repository the suite lays out, with the provider's components YAML rendered by the target that renders the release's.
  Lodia and etcd-druid are installed from Lodia's chart at the `LODIA_COMMIT` of the `Makefile`.
- A worker joins through Cluster API's kubeadm bootstrap provider with the bootstrap objects that Lodia's Tenant controller writes, and a pod runs on it, read through the `<cluster>-kubeconfig` that the provider wrote.
- A test outside the suite's tagged files that drives clusterctl, or loads the kubeconfig that `KUBECONFIG` names, first points `KUBECONFIG` at a kubeconfig of a cluster that never answers, and fails if that cluster is asked anything.
  The suite's tagged files, those with the build tag `e2e`, do not, and rely on the guard and on `E2E_KUBECONFIG`.
- Every wait has a bound in the suite's model, the bounds of each job add up to less than its `go test`'s `-timeout`, and `-timeout` to less than the job's `timeout-minutes`.
  A wait that takes its bound from the context it runs on runs on a context that `within` makes from a field of the bounds with `context.WithTimeoutCause`, and a failure that the end of the bound cuts short names the bound with `context.Cause`.
  No client of the suite has a client-side rate limiter: every `rest.Config` that it builds has its `QPS` set to -1, as controller-runtime's default is, and a test holds that each constructor of one does and that no function of the suite makes one that does not.
  A docker call, the dump, the unpause of `pauseCycle`, helm's `--wait --timeout 5m` inside `installLodia` and clusterctl's wait for the providers inside the install and the upgrade end at a bound of their own, which the model holds elsewhere ([`test/e2e/README.md`](../../test/e2e/README.md)).
- CI runs `make e2e` as the job `e2e`, `make e2e-upgrade` as the job `e2e-upgrade` and `make e2e-minor` as the job `e2e-minor`, on every pull request and every push to `main`, each on a kind cluster of its own, `capl-e2e`, `capl-e2e-upgrade` and `capl-e2e-minor`.
- `e2e-upgrade` installs through clusterctl the release that `test/pin/previous-release.yaml` names, pinned by the sha256 of its two files, joins a worker, and upgrades the provider to the tree's build with `clusterctl upgrade apply`: the new manager has to lead, and to leave what the release made as it was, the objects and the copied Secrets, the `Machine` and its Node, and the times of last transition of `Available`.
- `e2e-minor` starts a Cluster at v1.36.4 with a worker that joined, raises `spec.version` to v1.37.0, and creates a second MachineDeployment at v1.37.0.
  No `Machine` of it may exist while the `LodiaControlPlane` reports the old version, the `LodiaControlPlane` may not report v1.37.0 while Lodia still has an upgrade or a part below it, and the MachineSet has to say once that the control plane is upgrading; after the step the second worker joins at v1.37.0 and the first, `Machine` and Node, stays as it was ([the test](../../test/e2e/README.md#the-minor-step-test)).
- `make verify` compares the components of that release with the tree's render, and `make release-check` refuses a tag whose commit pins a release older than the newest one published below the tag, so that every release is upgrade-tested from the one before it.
- What the provider shares with Lodia's guard is Lodia's [decision of the same name](https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/docs/decisions/e2e-on-a-named-cluster.md).

## Why

- Lodia's own e2e cluster is `kind-lodia-e2e-local`, in which the suite would upgrade Lodia, so the two guards refuse each other's clusters.
- The goal is Lodia's roadmap, phase 3: a `Cluster` of this provider reaches `ControlPlaneInitialized`, and a `MachineDeployment` of Cluster API's Docker machines joins it through the kubeadm bootstrap provider, proved on real objects: the Node `Ready` and a pod running on it.
- Installing the YAMLs one object at a time leaves no clusterctl processing, label or inventory to test, and clusterctl's owner graph would see no `Cluster`.
  Through clusterctl, the path users take is the one tested, and the release's own render is what runs.
- On 2026-11-13 a mutant of the suite's clusterctl factory made clusterctl fall back to its own proxy, which loads the current context's kubeconfig, and a test run reached a live cluster.
  The trap turns such a fallback into a failing test.
- The upgrade is a job of its own: a management cluster holds one instance of the provider ([clusterctl](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L375-L379)), so the release could follow the join test on its cluster only through a delete and an install of the release, which no user does, and the join test's own `Init` of the tree's build, which it tests because users take that path, has to stay in the join job.
  The pin moves by hand and is checked at the tag, since the sums of a release exist only once it is published, and a test that fails once a newer release exists would fail every pull request for a cause outside it.
- The static comparison names in seconds what the e2e shows on real objects: an object that clusterctl's upgrade keeps and the render drops or renames stays in every upgraded cluster, a `roleRef` that changes fails the upgrade, since it is immutable ([kubernetes](https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/apis/rbac/validation/validation.go#L206)), and a CRD version that the release serves and the render drops or serves no more stops being served, which clusterctl refuses wherever a ClusterClass names it ([clusterctl](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L660-L676)).
- The minor step is a job of its own: the join test would start its Cluster at v1.36.4, so its fresh join would no longer be at the newest minor and the longest job would grow, and the upgrade test's Cluster is made by the release, whose failed upgrade leaves the provider scaled down or deleted, so a minor step could not run behind it.
- A step's own bound has to fail before `go test` gives up, since only the step's failure writes the artifacts.
- client-go builds the rate limiter of a REST client only for a QPS above zero ([`rest/config.go`](https://github.com/kubernetes/client-go/blob/v0.37.2/rest/config.go#L373-L386)), and a request of a client without one skips it ([`rest/request.go`](https://github.com/kubernetes/client-go/blob/v0.37.2/rest/request.go#L667-L670)).
  The configs that the suite takes from clientcmd and envconf kept 5 requests a second with a burst of 10, and the Proxy's 500 with 1000, and x/time's `Wait` refuses a request under a deadline whose token is due after it, with an error that is no context's ([`rate.go`](https://github.com/golang/time/blob/v0.15.0/rate/rate.go#L268-L277)), which a tick at the end of a window brings about.
  The first decision routed around that refusal, with `endAfter`, which ended a context by cancelling it, `cutShort`, which gave a failed call the cause that the cancel had, and three checks of the Go files of the suite that held both, with their tests and the fixtures of the limiter.
  With `QPS` at -1 on every config the cause is gone, and the waits end by the deadline of `context.WithTimeoutCause`.
  controller-runtime does the same by default and leaves the load to API priority and fairness ([`config.go`](https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.2/pkg/client/config/config.go#L101-L105)).

## Rejected

- Cluster API's `test/framework` with Ginkgo — its top-level helpers assume a `KubeadmControlPlane`, it forces Ginkgo, and it costs 42 new modules against 8 for `sigs.k8s.io/e2e-framework`, which Lodia's suite uses too ([Lodia's decision](https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/docs/decisions/e2e-on-a-named-cluster.md)).
- Waits that end by cancelling a context, with `endAfter`, `cutShort` and their checks, beside clients that keep client-go's rate limiter (decided 2026-11-09, replaced 2026-11-15): `QPS` at -1 removes what they routed around, at one line for each config.
- A patch step inside the join test, `spec.version` from v1.37.0 to v1.37.2 before the scale to 2 — the same gate of the MachineSet holds it with no second minor, but a patch step has no preflight and no `Backup` ([the steps](https://github.com/lodia-io/lodia/blob/f91168b1248dc18151a0c2d442fe2050593a0866/docs/decisions/upgrades-and-maintenance-windows.md?plain=1#L32)) and the worker is of the same minor, so it holds none of what only a minor brings.

## Consequences

- The suite needs Docker and helm, and runs where the Docker network `kind` is routable, as on GitHub's Linux runners.
  Docker Desktop does not route it, so the suite does not run on macOS.
- A new bound joins the model of the job whose test waits, `boundSet` for `e2e`, `upgradeBoundSet` for `e2e-upgrade` and `minorBoundSet` for `e2e-minor`, in `TestTheBoundsFitUnderTheTimeouts`, and a new wait is checked by `TestEveryWaitOfEveryJobsFileIsBoundedByAFieldOfItsOwnBoundsThatNoOtherWaitTakes`, whose subtest for each job also fails a wait on a context that `context.WithTimeout` made.
  The install's and the dump's bounds are `suiteBounds`, which every job adds.
- A new `rest.Config` of the suite has its `QPS` set to `clusterctlproxy.NoRateLimit`, or goes to the Proxy's `New` or `NewClient`, and `TestNoRESTConfigOfTheSuiteKeepsClientGosRateLimiter` fails one that does neither.
- The upgrade job reaches ghcr.io for the release's image and GitHub for its two files, and the pin moves by hand after each release ([CONTRIBUTING](../../CONTRIBUTING.md#release-a-version)); nothing in it holds the clusterctl variables, which equal the manifest's defaults there, and `test/components` does, with explicit values.

## Code

- `test/e2e/guard.go` — `RequireNamedCluster`; `test/e2e/guard_test.go` — `TestRequireNamedCluster`, `TestMakeAndTheGuardAgreeOnTheClusterName` and `TestMakeAcceptsOnlyNamesWhoseClusterLodiaAccepts`.
- `test/e2e/helm_test.go` — `refuseHelmKubeEnv` and `TestHelmKubeVariablesAreRefused`; `test/e2e/suite_test.go` calls it with the guard.
- `Makefile` — `e2e`, `e2e-upgrade`, `e2e-minor`, `e2e-cluster`, `e2e-lodia` and `upgrade-components`; `.github/workflows/ci.yml` — the jobs `e2e`, `e2e-upgrade` and `e2e-minor`.
- `test/e2e/join_test.go` — `TestAWorkerJoinsAClusterOfLodiasControlPlane`; `test/e2e/install_test.go` and `test/e2e/clusterctl_test.go` — the install; `test/e2e/README.md` — the steps and the failure modes.
- `test/clusterctlproxy/trap.go` — `TrapTheDefaultKubeconfig`; `test/clusterctlproxy/proxy_test.go` — `TestEveryCallAgainstAClusterThatDoesNotAnswerEndsWithinItsBoundAndOneDiscoveryTimeout` and `TestEveryClusterctlClientIsBuiltThroughTheFactory`.
- `test/e2e/bounds_test.go` — `TestTheBoundsFitUnderTheTimeouts`; `test/e2e/stable_test.go` — `within`.
- `test/clusterctlproxy/proxy.go` — `NoRateLimit`; `test/clusterctlproxy/proxy_test.go` and `test/e2e/ratelimit_test.go` — the tests that a client of each config has no rate limiter.
- `test/e2e/upgrade_test.go` — `TestTheProviderUpgradesFromThePreviousRelease`; `test/e2e/upgraded_test.go` — its checks; `test/e2e/lodia_upgrade_test.go` — the install of the release; `test/pin` — the pin and the download by sha256.
- `test/components/upgrade.go` — the comparison of `make verify`; `hack/releasecheck/check.go` — `requireUpgradePin`.
- `test/e2e/minor_test.go` — `TestAMinorStepHoldsNewWorkersUntilLodiaRunsIt`; `test/e2e/stepped_test.go` — its checks and the unit tests of them; `test/e2e/testdata/workers-next.yaml` — the second MachineDeployment.
