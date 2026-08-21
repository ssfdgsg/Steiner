# Steiner — LLM Inference Load Balancing and Scheduling Gateway

> 🌐 English | [简体中文](README.md)

> An OpenAI-compatible L7 gateway for vLLM and SGLang: metric-driven, cache-aware scheduling that raises the throughput and stability of LLM inference clusters.

Steiner converges multiple inference engines behind a unified OpenAI-compatible entry point and provides model-level routing, load balancing, KV-cache prefix affinity, PD disaggregation, observability, and multi-instance coordination. It targets teams that need to uniformly front and schedule LLM inference services in Kubernetes or self-hosted clusters.

**License:** [Apache-2.0](LICENSE) · **Contributing:** [CONTRIBUTING.md](CONTRIBUTING.md) · **Security:** [SECURITY.md](SECURITY.md)

## Quick Start

Prerequisites: Go 1.22; Node.js LTS if you plan to modify the admin console.

```bash
make test
make build
make smoke
```

Start a local demo environment (two mock backends + Prometheus) with Docker:

```bash
make up
# Console: http://localhost:8080/admin/ui/
```

Stop the demo environment with `make down`.

An L7 gateway for **vLLM / vLLM-Omni / SGLang / SGLang-Omni** inference engines that provides:

- **Multi-engine load balancing**: unified OpenAI-compatible entry; per-engine adapters hide protocol and metric differences; model-level routing (per-model backend pools / policies / rate limits), model-name rewriting (public name → backend deployment name), and weighted-split canary traffic;
- **Metric-driven scheduling**: dual-channel metric ingestion (direct scrape of backend `/metrics` + remote Prometheus PromQL), with second-level metric snapshot refresh;
- **Dynamic compilation of policy expressions**: scheduling formulas are written as expressions (expr-lang/expr), compiled to bytecode at runtime, and atomically hot-swapped without restart;
- **KV-cache prefix-aware routing**: an approximate radix tree maintains “prefix → backend” affinity to maximize prefix-cache hits, automatically falling back to load-first routing when unbalanced;
- **PD disaggregation and NCCL/NIXL link-group scheduling**: prefill/decode role instances are paired, and requests are dispatched only between P–D pairs that have an established KV transfer channel (NCCL / NIXL / Mooncake);
- **Webhook alerting and autoscaling signals**: alert rules (sharing the same expression variables as scheduling formulas) are evaluated periodically through a pending/firing/resolved state machine and pushed to DingTalk / Feishu / WeCom / Slack / generic webhooks; an autoscaler produces desired-replica recommendations for external controllers (K8s operator / HPA / KEDA) to act on;
- **Multi-instance horizontal deployment**: Redis as the coordination layer — distributed rate limiting (GCRA, cluster-wide shared model quota), session-stickiness sharing, policy hot-reload broadcast, single-leader alert/autoscale evaluation, and an `/admin/cluster` member view; on Redis failure the gateway degrades to local behavior (**availability trade-off**: under fail-open each instance falls back to its local token bucket, so the cluster quota can be exceeded up to N× during the outage; set `rate_limit_fail_open: false` to deny instead and keep the quota strict);
- **Distributed tracing (OpenTelemetry)**: one span chain per request (queue wait / each routing decision / each retried forward / TTFT event / token usage), exported via OTLP, with `traceparent` injected upstream to continue the full trace on the engine side; `X-Trace-Id` / `X-Request-Id` response headers correlate with logs and metrics;
- **Dynamic backend registration and config persistence**: `POST/DELETE /admin/backends` adds/removes backends at runtime (lock-free copy-on-write pool hot-swap); changes are persisted to PG/MySQL and restored on restart, and synced to all instances via cluster broadcast — closing the loop with the autoscaler: an external controller scales up, then calls the admin API to start serving traffic, with no restart;
- **React admin console**: `/admin/ui/` works out of the box (the build is embedded in the binary; no static server needed; the static shell loads publicly, then prompts for `server.admin_token` — the token lives only in the browser localStorage) — one-click scheduling-preset switching, live backend load and add/remove/cordon, a scheduling explainer (answering “why was this routed to X”), and runtime views for KV / queue / PD links / alerts / autoscale / cluster;
- **Streaming passthrough, token-bucket rate limiting, session stickiness, circuit-breaker ejection, and self-observability**.

## Architecture Overview

```
                          ┌────────────────────────────────────────────────────┐
                          │                      Steiner                       │
  Client                  │                                                    │
  (OpenAI SDK) ──────────▶│  server ──▶ proxy(features/ratelimit) ──▶ scheduler│
  /v1/chat/completions    │   /v1/*       │                       │   ▲        │
  /v1/completions         │               │        ┌──────────────┘   │        │
                          │               │        ▼                  │        │
                          │               │  8 built-in strategies    │        │
                          │               │  policy(expr VM/hot swap) │        │
                          │               │  kvcache(radix prefix tree)│       │
                          │               │  session(consistent hash) │        │
                          │               │  pd(P–D link groups)      │        │
                          │               │            │              │        │
                          │               │            ▼              │        │
                          │               │      backend registry ────┘        │
                          │               │      (health/breaker/cordon)       │
                          │               │            ▲                       │
                          │               │   metrics(scrape+PromQL+mapping)   │
                          │               ▼                                    │
                          │    proxy forward(SSE/TTFT/retry-before-TTFB/PD)    │
                          │                                                    │
                          │  admin API(policy hot reload/cordon/explain/kv/pd) │
                          │  alerting(state machine/autoscale) ──▶ webhooks    │
                          │  cluster(election/ratelimit/session) ◀─▶ Redis     │
                          │  store(backends/policies) ◀─▶ PostgreSQL/MySQL     │
                          └───────────────┬────────────────┬───────────────────┘
                                          ▼                ▼
                               vLLM / vLLM-Omni      SGLang / SGLang-Omni
                               (incl. PD disaggregation: prefill pool + decode pool)
```

## Request Lifecycle

1. **Ingress** (`internal/server`): parse the route and read the request body (bounded by `max_body_bytes`);
2. **Feature extraction** (`internal/proxy/parse.go`): model name, streaming flag, prompt text (used for prefix matching; multimodal requests use text segments), session key, priority;
3. **Admission** (`internal/proxy`): model-level token-bucket rate limiting fails fast on overflow; when no capacity is available, requests queue briefly via `internal/queue` (429 when the queue is full, 503 on timeout);
4. **Candidate filtering**: the backend registry yields candidates that are “healthy && serve this model && not cordoned && PD role matches”;
5. **Scoring and selection** (`internal/scheduler`): a session-stickiness hit short-circuits; otherwise the active policy (built-in or expression) scores every candidate, combined with `internal/kvcache` prefix-match ratios and `internal/pd` link-group constraints, to pick the target backend (a P–D pair in PD mode);
6. **Forwarding** (`internal/proxy`): rewrite the request (model-name mapping, PD bootstrap injection), SSE streaming passthrough; failures before the first byte may retry on another backend;
7. **Write-back**: update the radix tree, inflight counters, and the gateway's own metrics (`internal/metrics`).

## Directory Navigation

| Path | Responsibility (see each directory's README) |
|---|---|
| `cmd/gateway/` | Process entry point: wires modules, starts HTTP servers and background loops |
| `configs/` | YAML config examples and a full option reference |
| `api/` | External HTTP protocol: data plane (OpenAI-compatible) and management plane (admin API) |
| `internal/config/` | Config loading, validation, hot-reload dispatch |
| `internal/server/` | HTTP routing, middleware, admin API, graceful shutdown |
| `internal/proxy/` | Reverse proxy, SSE streaming, retries, request rewriting |
| `internal/backend/` | Backend abstraction and registry (incl. split sub-pools), health checks, circuit-breaker ejection (engine differences are concentrated in the metrics mapping table and the proxy PD protocol) |
| `internal/metrics/` | Metrics subsystem: `/metrics` direct scrape, PromQL remote queries, unified snapshot, authoritative table of self metrics |
| `internal/policy/` | Policy expression engine: compilation, hot swap, variable table (the single authoritative source) |
| `internal/scheduler/` | Scheduling core: eight built-in strategies and scoring explanations |
| `internal/kvcache/` | Prefix-aware routing: radix tree, affinity, eviction, fallback on imbalance |
| `internal/pd/` | PD disaggregation orchestration and NCCL/NIXL/Mooncake link-group management |
| `internal/alerting/` | Webhook alerts (rule state machine + multi-template notifications) and autoscale recommendations |
| `internal/session/` | Session stickiness (improves KV hits for multi-turn conversations) |
| `internal/cluster/` | Multi-instance coordination: leader election, distributed rate limiting, session sharing, policy/backend change broadcast (Redis) |
| `internal/store/` | Dynamic-config persistence: backend/policy persistence and startup recovery (PostgreSQL / MySQL) |
| `internal/queue/` | Capacity queueing (event-driven wakeup + fallback polling) |
| `internal/tracing/` | OpenTelemetry setup: OTLP export, sampling, W3C propagation (instrumentation lives in proxy) |
| `web/` | React admin console source (Vite + TS, build embedded in the binary) |
| `internal/webui/` | Console build embedding and static hosting (`/admin/ui/`) |
| `pkg/openai/` | OpenAI protocol types (reusable by external tools; rate limiting lives in proxy, self metrics in metrics) |
| `test/` | Integration tests and mock backends for all four engines |
| `deploy/` | Docker Compose / K8s / Grafana deployment artifacts |
| `docs/` | Architecture decision records (ADRs) |
| `scripts/` | Local verification scripts (build, test, smoke) |

## Technology Choices (Ecosystem-First)

| Capability | Choice | Rationale |
|---|---|---|
| Expression engine | `expr-lang/expr` | De-facto community standard: compiles to bytecode, type-checks, supports timeouts and environment allowlists — “dynamically compiled formulas” that stay safe and controlled |
| Metric text parsing | `prometheus/common/expfmt` | Official exposition-format parser; direct backend `/metrics` scraping with zero custom code |
| PromQL queries | `prometheus/client_golang/api/v1` | Official HTTP API client |
| Self metrics | `prometheus/client_golang` | Official SDK |
| Config | `gopkg.in/yaml.v3` | De-facto standard |
| Cluster coordination | `redis/go-redis` + `go-redis/redis_rate` (GCRA rate limiting) | Official client and an author-maintained rate-limiting library; verified locally with `miniredis` in tests |
| Config persistence | stdlib `database/sql` + `jackc/pgx` (PG) / `go-sql-driver/mysql` | Both officially recommended drivers; dialect differences are isolated in one place, verified locally with `sqlmock` |
| HTTP routing/proxy | stdlib `net/http` (1.22 method routing; hand-written forwarding to support retry-on-another-backend / PD two-phase / TTFT instrumentation) | Zero framework dependency |
| Radix tree | `armon/go-radix` considered first; a documented in-house implementation if eviction/multi-tenant metadata requirements are not met (see `internal/kvcache/README.md`) | — |

## Ecosystem References

- **sglang-router**: cache-aware routing (approximate radix tree + absolute/relative imbalance fallback thresholds) and PD routing (requests dispatched to P and D simultaneously, carrying bootstrap info);
- **vLLM production-stack router**: session stickiness, KV-aware routing;
- **AIBrix Gateway / Envoy AI Gateway**: gateway-level rate limiting, model-routing shape.

## Build and Verify

```bash
make build      # compile (GOTOOLCHAIN=local)
make test       # unit tests + integration tests (test/integration: dual gateway + miniredis)
make smoke      # local smoke test: start mock backends + gateway
make web        # build the React console (only needed when changing the frontend; dist is committed, go build does not require Node)
make web-dev    # frontend hot-reload dev (Vite :5173 proxied to the local gateway :8080)
make bench      # performance baselines: scheduling/prefix-tree/forwarding (baseline in docs/perf-baseline.md)
make conformance # real-engine verification: docker-run official vLLM CPU image + small model,
                 # verifying metric-name mapping/SSE framing/usage extraction/model-name rewrite/health checks
                 # (first run downloads the image and model; KEEP_UP=1 keeps the environment for debugging)
```

> Local note: under WSL2, `GOTOOLCHAIN` must be pinned to `local` (go 1.22.2); the Makefile does this to avoid the toolchain auto-download hanging.
