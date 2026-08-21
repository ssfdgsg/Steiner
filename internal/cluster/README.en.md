# internal/cluster — Multi-Instance Coordination Layer

> 🌐 English | [简体中文](README.md)

Uses Redis as the single coordination store so that multiple gateway instances can be deployed horizontally. With `cluster.enabled=false` (the default) this module is completely out of the picture and behavior matches single-instance mode.

## Capabilities

| Capability | Mechanism | File |
|---|---|---|
| Instance registration & member view | Heartbeat key `{prefix}:instance:{id}` (auto-expiring TTL); `GET /admin/cluster` queries it | `cluster.go` |
| Leader election | NX+TTL lease on `{prefix}:leader`, atomically renewed with Lua; only the leader evaluates alerts and autoscale | `cluster.go` |
| Distributed rate limiting | GCRA (`go-redis/redis_rate`); all instances share the `rate_limit_qps` quota per model | `ratelimit.go` |
| Session-stickiness sharing | Binding table lives in Redis (sliding TTL) for cross-instance consistency; a local table keeps a hot copy | `session.go` |
| Policy hot-reload broadcast | After an admin PUT, synced to all instances via the pub/sub channel `{prefix}:policy` | `policy.go` |
| Backend-change broadcast | After admin add/remove of backends, synced via `{prefix}:backends` (only the initiating instance persists) | `backends.go` |

## Degradation Principles

Redis failure does not affect the data plane:

- Rate limiting falls back to the local token bucket (fail-open; total allowance temporarily becomes quota × instance count);
- Session stickiness falls back to the local hot copy (Bind writes both; Lookup checks Redis first, then local);
- Election/broadcast/member view pause; the leader determination keeps its pre-failure state (the lease cannot be stolen either);
- All failures are counted in `gateway_cluster_redis_errors_total{op}` and converge automatically after recovery.

Startup exception: with `cluster.enabled=true` but Redis unreachable, startup fails outright (shift-left exposure of config problems) instead of running sick.

## State That Stays Local Per Instance (design trade-off)

- **KV prefix tree**: each instance independently learns “prefix → backend” affinity. The cost of keeping a radix tree in sync across instances far outweighs the benefit; the backends' real prefix-cache hit rates are already a global signal via direct metric scraping, and cache_aware's imbalance fallback self-corrects.
- **Inflight counts**: local values only reflect this instance's in-flight load; least_request/p2c and friends rely mainly on directly scraped running/waiting (the global truth), with local inflight as a secondary signal.
- **Circuit-breaker/health state**: each instance probes and ejects independently, which naturally tolerates single-instance misjudgment.

## Session Invalidation

On a backend health flip, `session.Store.InvalidateBackend` only clears local bindings; stale bindings in Redis are not actively cleaned — proxy always checks `backend.Available` before using a binding, stale bindings are skipped and rebound after the next successful forward. This is lazy convergence.
