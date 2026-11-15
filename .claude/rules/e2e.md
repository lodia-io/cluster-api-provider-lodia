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
- A wait that takes its bound from the context it runs on (`pollUntil`, `apply`, `clusterctlproxy.MoveUntilBlocked` and the others that [`test/e2e/README.md`](../../test/e2e/README.md) lists) runs on a context that the same step made with `within` from a field of the bounds, and a step that reports the failure of a call on it joins `context.Cause` of that context to the error, so that the failure names the bound ([decision](../../docs/decisions/e2e-on-a-named-cluster.md)).
  `TestEveryWaitOfEveryJobsFileIsBoundedByAFieldOfItsOwnBoundsThatNoOtherWaitTakes` runs a subtest named by its file for each job of `jobs`, and fails a `context.WithTimeout` or a `context.WithTimeoutCause` in the place of `within`.
  `TestTheInstallRunsItsWaitsOnAContextThatEndsAtTheBoundOfTheInstall` holds the install the same way, and `TestNoFileWithABuildTagMakesAContextThatNoBoundEnds` fails a `context.Background`, `context.TODO` or `context.WithoutCancel` in a tagged file.
  The checks read calls written `context.X`, and follow no package imported under another name or by a dot import.
  A docker call, the dump, the unpause of `pauseCycle`, helm's `--wait --timeout 5m` inside `installLodia` and clusterctl's wait for the providers inside the install and the upgrade end at a bound of their own, which the README gives.
- No client of the suite has a client-side rate limiter: a new `rest.Config` of the suite has its `QPS` set to `clusterctlproxy.NoRateLimit`, or goes to `clusterctlproxy.New` or `clusterctlproxy.NewClient`, which set it.
  `TestNoRESTConfigOfTheSuiteKeepsClientGosRateLimiter` fails a function of `test/e2e` or of the Proxy that makes a config and does neither, and a new constructor of a config gets a test that a client of it has none, as `TestTheConfigOfTheDumpHasNoRateLimiter` has.
- A new bound is a field of the set of the test that waits in `test/e2e/bounds_test.go`, `boundSet` for the join test, `upgradeBoundSet` for the upgrade test and `minorBoundSet` for the minor step test, so that `TestTheBoundsFitUnderTheTimeouts` still holds the sum of each job under the `-timeout` of its target in the `Makefile` and under the job's `timeout-minutes` in `.github/workflows/ci.yml`.
- A new job that runs the suite is a build tag of its own, a target with its own `-timeout`, a kind cluster and directory of its own, a set of bounds, an entry of `jobs` in `bounds_test.go`, which names the job's target, tags, test, file and owner and holds its set of bounds, and which `TestTheBoundsFitUnderTheTimeouts` requires for every target of the `Makefile` that runs `go test` with `-tags e2e`, and holds each job of `ci.yml` to make its own target alone, a run of `make lint` with its tag set, which `TestEveryJobIsLintedWithItsBuildTags` requires, and a test that its target's `-run` names in the file that the entry names, whose build tags that job's tags satisfy and no other job's do, and a file that declares no test which the `-run` leaves out, all of which `TestTheBoundsFitUnderTheTimeouts` requires, and which `TestEveryWaitOfEveryJobsFileIsBoundedByAFieldOfItsOwnBoundsThatNoOtherWaitTakes` reads as the subtest named by that file; the tests that drive clusterctl hold `ApplyUpgrade` as they hold `Init` and `Move`, and a new call joins them.
- The suite accepts only its own kind cluster, whose context starts with `kind-capl-e2e`, never Lodia's `kind-lodia-e2e*`; a new target creates its cluster, `capl-e2e-upgrade` for `make e2e-upgrade` and `capl-e2e-minor` for `make e2e-minor`, after `make e2e-cluster` accepted the name ([`test/e2e/README.md`](../../test/e2e/README.md)).
