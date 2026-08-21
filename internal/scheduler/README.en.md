# internal/scheduler/ — Scheduling Core

> 🌐 English | [简体中文](README.md)

## Responsibility

Given request features and the model route, pick the forwarding target from the candidate backends. All policies share the same candidate filter: healthy, not manually cordoned, not in a circuit-breaker cooldown, not excluded by this retry, and with concurrency quota available.

## Eight Policies (dispatched via `Pick` / `PickAmong`; implementations in scheduler.go / ring.go)

| Policy | Semantics |
|---|---|
| `round_robin` | Round robin |
| `random` | Random |
| `weighted_random` | Random by static weight |
| `least_request` | Least composite load (gateway inflight + engine running + engine waiting) |
| `p2c` | Pick the lesser load of two random choices (Power of Two Choices) |
| `consistent_hash` | Consistent hashing (key: session ID > prompt > model name; 64 virtual nodes per backend, clockwise fallback when unavailable) |
| `cache_aware` | KV prefix-aware: affinity route when the highest hit rate ≥ `match_threshold` and the target is not unbalanced; otherwise fall back to least load (imbalance = abs+rel dual thresholds, aligned with sglang-router) |
| `expression` | Policy-expression scoring (dynamically compiled by the policy engine; lowest score wins; degrades to least load when everything is filtered out) |

## Cooperation with Other Modules

- **kvcache**: `prefixMatches` computes the per-backend prefix hit ratio, used as the `prefix_match` expression variable and as the cache_aware decision input; after a successful forward, proxy calls `Observe` to write back prefix ownership;
- **policy**: `BuildEnv` builds the evaluation environment (the variable table is the single authoritative source: `internal/policy/README.en.md`);
- **pd**: the prefill side of a PD group reuses `PickAmong` (all policies available);
- **Session stickiness**: short-circuited by proxy before scheduling (`internal/session`); this package is unaware of it;
- **Canary splits**: when a route carries splits, first pick a sub-pool by weight, then pick within it (availability outranks split ratios).

## Explainability

`Explain(route, policy, req)` returns a per-backend score breakdown (ascending), exposed via `GET /admin/explain`.

## Files

| File | Description |
|---|---|
| `scheduler.go` | Policy dispatch, candidate filtering, expression environment, Explain |
| `ring.go` | Consistent-hash ring (cached by candidate-set signature) |
| `scheduler_test.go` | All policies and edge-case tests |
