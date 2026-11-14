---
paths:
  - "test/**"
  - ".github/workflows/**"
  - "Makefile"
  - "internal/controller/move_test.go"
---

# The e2e suite and the tests that drive clusterctl

- A test outside the tagged suite that drives clusterctl, or loads the kubeconfig that `KUBECONFIG` names, calls `clusterctlproxy.TrapTheDefaultKubeconfig(t)` first, as the tests of `test/e2e/clusterctl_test.go`, `test/clusterctlproxy` and `internal/controller/move_test.go` do: on 2026-11-13 a mutant's fallback to clusterctl's own proxy loaded the current context and reached a live cluster ([decision](../../docs/decisions/e2e-on-a-named-cluster.md)).
  The tagged suite, `suite_test.go`, `install_test.go` and `join_test.go`, calls it nowhere and relies on its guard and on `E2E_KUBECONFIG`.
- A wait that takes its bound from the context it runs on (`pollUntil`, `apply`, `clusterctlproxy.MoveUntilBlocked` and the others that [`test/e2e/README.md`](../../test/e2e/README.md) lists) runs on a context that the same step made with `endAfter` from a field of the bounds, so that it ends by cancelling the context and never by a deadline: client-go's rate limiter refuses a request whose token falls after its deadline, with an error that is no context's.
  `TestEveryWaitOfTheJoinTestIsBoundedByAFieldOfTheBoundsThatNoOtherWaitTakes` fails a `context.WithTimeout` in its place.
  A docker call, the dump, the unpause of `pauseCycle` and clusterctl's wait for the providers inside the install still end by a deadline, for the reasons the README gives.
- A new bound is a field of `boundSet` in `test/e2e/bounds_test.go`, so that `TestTheBoundsFitUnderTheTimeouts` still holds the sum under `-timeout` in the `Makefile` and under the job's `timeout-minutes` in `.github/workflows/ci.yml`.
- The suite accepts only its own kind cluster, whose context starts with `kind-capl-e2e`, never Lodia's `kind-lodia-e2e*`; a new target creates its cluster after `make e2e-cluster` accepted the name ([`test/e2e/README.md`](../../test/e2e/README.md)).
