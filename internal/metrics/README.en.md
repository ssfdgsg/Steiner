# internal/metrics/ — Metrics Subsystem (Dual Channel + Self Export)

> 🌐 English | [简体中文](README.md)

## Responsibility

```
backend /metrics ──(Scraper, second-level)──▶ backend.Snapshot (atomic pointer publish) ─▶ lock-free reads on the scheduling hot path
Prometheus      ──(PromCollector)──────────▶ backend.PromVars (expression vars["..."]) ──┘
gateway itself  ──(Gateway metric set + BackendCollector)──▶ GET /metrics
```

Channel positioning: direct scraping covers **low-latency core signals** (running/waiting/KV usage, second-level freshness); PromQL covers **external signals the engines cannot expose** (DCGM GPU utilization, temperature, network bandwidth, etc.).

## Direct Scraping (scraper.go + adapters.go)

- Parses backend exposition text with the official `prometheus/common/expfmt` parser;
- Same-name multi-series (multiple model labels) are summed; counters are window-differenced to produce `rate:<metric name>` derived values;
- Normalized fields are selected by the per-engine-family candidate-name table in `adapters.go` (new and old metric names coexist; the first hit in order is taken); when the hit-rate gauge is missing, it is derived from the hits/queries counter rate pair;
- A failed scrape keeps the old values and only appends an `Err` marker, avoiding metric flicker that would jitter scheduling.

Normalized fields: `Running` / `Waiting` / `KVUsage` / `HitRate` / `GenTokPerSec` / `Raw` (full). Metric-name mapping table (the single authoritative source is the `adapters.go` source):

| Field | vllm family candidates | sglang family candidates |
|---|---|---|
| Running | `vllm:num_requests_running` | `sglang:num_running_reqs` |
| Waiting | `vllm:num_requests_waiting` | `sglang:num_queue_reqs` |
| KVUsage | `vllm:kv_cache_usage_perc`, `vllm:gpu_cache_usage_perc` | `sglang:token_usage`, `sglang:kv_cache_usage` |
| HitRate | `vllm:gpu_prefix_cache_hit_rate`; fallback rate(hits)/rate(queries) | `sglang:cache_hit_rate` |
| GenTokPerSec | rate(`vllm:generation_tokens_total`) | `sglang:gen_throughput` (gauge, direct read) |

## PromQL Bypass (promql.go)

- The official `client_golang/api/v1` client periodically executes configured queries (instant vectors);
- Results are matched to backends by the `backend_label` tag value (preferring a same-name entry in the backend `labels`, then the URL host);
- The variable table is rebuilt wholesale each round, so decommissioned series leave no stale values behind.

## Self Metrics (exporter.go + collector.go; this table is the single authoritative source)

All are registered in a dedicated Registry and exported via `GET /metrics`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `gateway_requests_total` | counter | backend,model,code | Total forwarded requests |
| `gateway_request_duration_seconds` | histogram | backend,model | End-to-end request latency |
| `gateway_time_to_first_byte_seconds` | histogram | backend,model | First-byte latency (TTFT for streaming) |
| `gateway_retries_total` | counter | — | Retries that switched backends |
| `gateway_rate_limited_total` | counter | model | Model-level rate-limit rejections |
| `gateway_pick_errors_total` | counter | model,reason | Pick failures (no_route/no_backend/queue_full/queue_timeout/exhausted/pd_*) |
| `gateway_pick_duration_seconds` | histogram | strategy | Per-pick duration |
| `gateway_upstream_errors_total` | counter | backend,kind | Upstream error classes (connect/bad_status/stream) |
| `gateway_prompt_tokens_total` / `gateway_completion_tokens_total` | counter | backend,model | Upstream usage token counts (last chunk for streaming) |
| `gateway_backend_healthy` / `gateway_backend_inflight` | gauge | backend | Health state / gateway-side inflight (5s refresh) |
| `gateway_kvtree_bytes` / `gateway_kvtree_nodes` | gauge | — | Prefix-tree size |
| `gateway_queue_depth` | gauge | model | Queue depth (live read at scrape, per route) |
| `gateway_pd_link_inflight` | gauge | prefill,decode | Inflight KV transfers on PD links |
| `gateway_split_requests_total` | counter | model,split | Canary split hits |
| `gateway_backend_info` | gauge | backend,engine,url | Backend metadata (constant 1) |
| `gateway_backend_running_requests` / `_waiting_requests` | gauge | backend | Normalized engine-load view |
| `gateway_backend_kv_cache_usage` / `_prefix_hit_rate` / `_gen_tokens_per_second` | gauge | backend | KV usage / prefix hit rate / generation throughput |
| `gateway_backend_scrape_up` | gauge | backend | Whether the latest direct scrape succeeded |
| `gateway_alerts_firing` | gauge | rule | Firing instance count per rule |
| `gateway_webhook_sent_total` | counter | target,outcome | Webhook delivery outcomes |
| `gateway_autoscale_desired_replicas` | gauge | model | Recommended replica count (usable as an HPA/KEDA external metric) |
| `gateway_build_info` | gauge | version | Build info (constant 1) |

(Cluster mode adds `gateway_cluster_*` metrics; see exporter.go.)
The normalized view (`gateway_backend_*`) is served by a custom Collector that reads the atomic snapshot live at scrape time: Prometheus only needs to scrape the gateway to see the whole cluster's engine load, and removed backends leave no stale series.

## Admin Aggregation and Time Series (stats.go + history.go)

The console needs “current throughput, latency percentiles, trends over the last hours” — but it should not be forced to depend on an external Prometheus just to show basic KPIs. Therefore:

- `Gateway.Aggregate()` in `stats.go` calls `Registry.Gather()` directly — **same source, same semantics as `/metrics`**, adding no runtime counting state. Percentiles are estimated by linear interpolation over histogram cumulative buckets; precision is bounded by bucket edges and is meant for trend judgment, not SLA calculation; `code >= 400` and non-numeric codes count as errors.
- `History` in `history.go` is an in-process ring buffer (default 15s × 1440 ≈ 6 hours; see the constant in `cmd/gateway/main.go`), sampled periodically with counters differenced to obtain instantaneous RPS / error rate / interval-average latency; runtime states not present in self metrics (queue depth, available instance count, KV size, etc.) are sampled at the same instant via the injection-time `RuntimeProbe`, keeping all dimensions time-aligned. The first sample is a baseline and is not stored.
- Boundaries: the buffer is not persisted and not aggregated across instances — it clears on restart. For long-period, high-fidelity, multi-replica time series, Prometheus remains the source of truth; the console keeps a `/metrics` entry next to the charts.

## Files

| File | Description |
|---|---|
| `adapters.go` | Engine family → normalized-field metric-name mapping table |
| `scraper.go` | Direct-scrape loop (expfmt parsing, summing, differencing, hit-rate derivation) |
| `promql.go` | External Prometheus periodic queries and variable injection |
| `exporter.go` | Gateway self-metric set |
| `collector.go` | Backend normalized view / route-split counters |
| `stats.go` | Admin aggregation stats (Registry aggregation, histogram percentile estimation) |
| `history.go` | Admin time-series ring buffer and runtime probe |
| `scraper_test.go` | Dual-engine parsing, rate derivation, keep-old-on-failure, counter hit-rate cases |
| `stats_test.go` | Aggregation semantics, differencing semantics, buffer eviction cases |
