---
paths:
  - "internal/controller/**"
---

# The controller

- An object that the provider applies, or copies from, is read past the cache, through `Reconciler.APIReader` (the manager's API reader), before the pass decides on it, as `currentControlPlane`, `copySecrets` and `applySecret` do: [the decision](../../docs/decisions/objects-of-another-controller-are-left-alone.md) says why.
