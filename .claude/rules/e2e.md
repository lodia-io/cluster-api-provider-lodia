---
paths:
  - "test/**"
  - ".github/workflows/**"
  - "Makefile"
  - "internal/controller/move_test.go"
---

# The e2e suite and the tests that drive clusterctl

- A test outside the tagged suite that drives clusterctl, or loads the kubeconfig that `KUBECONFIG` names, calls `clusterctlproxy.TrapTheDefaultKubeconfig(t)` first, as the tests of `test/e2e/clusterctl_test.go`, `test/clusterctlproxy` and `internal/controller/move_test.go` do: on 2026-11-13 a mutant's fallback to clusterctl's own proxy loaded the current context and reached a live cluster ([decision](../../docs/decisions/e2e-on-a-named-cluster.md)).
  The tagged suite, the files of `test/e2e` with the build tag `e2e`, calls it nowhere and relies on its guard and on `E2E_KUBECONFIG`.
- A wait that takes its bound from the context it runs on (`pollUntil`, `apply`, `clusterctlproxy.MoveUntilBlocked` and the others that [`test/e2e/README.md`](../../test/e2e/README.md) lists) runs on a context that the same step made with `endAfter` from a field of the bounds, so that it ends by cancelling the context and never by a deadline: client-go's rate limiter refuses a request whose token falls after its deadline, with an error that is no context's.
  `TestEveryWaitOfTheJoinTestIsBoundedByAFieldOfTheBoundsThatNoOtherWaitTakes` and `TestEveryWaitOfTheUpgradeTestIsBoundedByAFieldOfItsOwnBoundsThatNoOtherWaitTakes` fail a `context.WithTimeout` in its place.
  `TestTheInstallRunsItsWaitsOnAContextThatEndAfterEndsAtTheBoundOfTheInstall` holds the install the same way, and `TestTheFailureOfACallOnAContextThatEndAfterMadeGoesThroughCutShort` that the failure of a call on such a context goes through `cutShort(bounded, err)`, except for `apply` and `applyTheCluster`, which [`test/e2e/README.md`](../../test/e2e/README.md) names, so that a step which reports it with `t.Fatal(err)`, `t.FailNow` or `require` fails a test.
  A function that returns no error, or cuts short itself, is listed in `namesItsCause` in `test/e2e/endafter_test.go`, and in `cutsShortInside` as well when its body is to be read.
  `TestNoFileWithABuildTagMakesAContextThatEndsByADeadline` fails a `context.WithTimeout` or a `context.WithDeadline` in a tagged file, in a function that a step calls as well.
  A docker call, the dump, the unpause of `pauseCycle` and clusterctl's wait for the providers inside the install and the upgrade still end by a deadline, for the reasons the README gives.
- A new bound is a field of the set of the test that waits in `test/e2e/bounds_test.go`, `boundSet` for the join test and `upgradeBoundSet` for the upgrade test, so that `TestTheBoundsFitUnderTheTimeouts` still holds the sum of each job under the `-timeout` of its target in the `Makefile` and under the job's `timeout-minutes` in `.github/workflows/ci.yml`.
- A new job that runs the suite is a build tag of its own, a target with its own `-timeout`, a kind cluster and directory of its own, a set of bounds and an entry of `jobs` in `bounds_test.go`, which `TestTheBoundsFitUnderTheTimeouts` requires for every target of the `Makefile` that runs `go test` with `-tags e2e`, and holds each job of `ci.yml` to make its own target alone; the tests that drive clusterctl hold `ApplyUpgrade` as they hold `Init` and `Move`, and a new call joins them.
- The suite accepts only its own kind cluster, whose context starts with `kind-capl-e2e`, never Lodia's `kind-lodia-e2e*`; a new target creates its cluster, `capl-e2e-upgrade` for `make e2e-upgrade`, after `make e2e-cluster` accepted the name ([`test/e2e/README.md`](../../test/e2e/README.md)).
