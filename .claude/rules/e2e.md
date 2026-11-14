---
paths:
  - "test/**"
  - ".github/workflows/**"
---

# The e2e suite and the tests that drive clusterctl

- A test that drives clusterctl or loads a kubeconfig calls `clusterctlproxy.TrapTheDefaultKubeconfig(t)` first: on 2026-11-13 a mutant's fallback to clusterctl's own proxy loaded the current context and reached a live cluster ([decision](../../docs/decisions/e2e-on-a-named-cluster.md)).
- A wait in the suite ends through `endAfter`, the suite's own end of its window, never a context deadline: client-go's rate limiter refuses a request whose token falls after its deadline, with an error that is no context's ([`test/e2e/README.md`](../../test/e2e/README.md)).
  `TestEveryWaitOfTheJoinTestIsBoundedByAFieldOfTheBoundsThatNoOtherWaitTakes` fails a `context.WithTimeout` in its place.
- A new bound is a field of `boundSet` in `test/e2e/bounds_test.go`, so that `TestTheBoundsFitUnderTheTimeouts` still holds the sum under `-timeout` in the `Makefile` and under the job's `timeout-minutes` in `.github/workflows/ci.yml`.
- The suite accepts only its own kind cluster, whose context starts with `kind-capl-e2e`, never Lodia's `kind-lodia-e2e*`; a new target creates its cluster after `make e2e-cluster` accepted the name ([`test/e2e/README.md`](../../test/e2e/README.md)).
