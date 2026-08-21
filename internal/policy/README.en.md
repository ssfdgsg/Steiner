# internal/policy/ — Policy Expression Engine

> 🌐 English | [简体中文](README.md)

## Responsibility

Dynamically compiles **scheduling formulas** (strings submitted via config or the admin API) into executable programs that the scheduler evaluates per candidate backend on the hot path. Core features:

- **Dynamic compilation**: built on `expr-lang/expr`, compiles to bytecode (`vm.Program`); evaluation is on the order of hundreds of nanoseconds;
- **Hot update**: `PUT /admin/policies/{name}` atomically replaces only on successful compilation; failures return 400 and leave the active policy unchanged;
- **Safe degradation**: an evaluation error on a single backend (e.g. division by zero) only skips that backend; when no candidates survive the filter, the scheduler degrades to least-load (`least_request` semantics), so requests never fail wholesale because a policy is too strict.

## Expression Model

One policy = `filter` (bool; false eliminates the candidate; empty string defaults to `healthy`) + `score` (numeric; **lowest score wins** — think of score as “cost”).

```yaml
policies:
  default:
    filter: "healthy && kv_usage < 0.98"
    score: "running * 2.0 + waiting * 6.0 + inflight * 1.0 + kv_usage * 8.0 - prefix_match * 10.0"
```

## Variable Table (evaluation environment; the single authoritative source; implemented in scheduler.BuildEnv)

| Variable | Type | Description |
|---|---|---|
| `model` | string | Request model name |
| `stream` | bool | Whether the request is streaming |
| `prompt_len` | float | Prompt text length in bytes |
| `prompt_tokens` | float | Prompt token count estimated by the gateway |
| `priority` | float | `priority` field from the request body (default 0) |
| `session` | string | Session key (X-Session-Id header or the body `user` field) |
| `is_multimodal` | bool | Whether the request has image, audio, or video inputs |
| `image_count` / `audio_count` / `video_count` | float | Count of each modality input |
| `backend` | string | Candidate backend ID |
| `engine` | string | vllm / vllm_omni / sglang / sglang_omni |
| `engine_family` | string | vllm / sglang |
| `weight` | float | Backend static weight |
| `healthy` | bool | Result of active health checks |
| `inflight` | float | Requests in flight on the gateway side |
| `running` | float | Requests currently executing on the engine (normalized direct scrape) |
| `waiting` | float | Requests queued on the engine |
| `kv_usage` | float | KV cache usage (0–1) |
| `hit_rate` | float | Prefix cache hit rate (0–1; gauge read directly, or derived from a counter rate pair) |
| `gen_tps` | float | Generation throughput in tokens/s (gauge read directly, or derived from a counter rate) |
| `prefix_match` | float | Gateway prefix-tree hit ratio for this backend (0–1) |
| `ttft_ewma` | float | Exponential moving average of gateway-measured first-byte latency (seconds, α=0.2); an engine-agnostic latency feedback signal; 0 when no samples exist yet |
| `preempt_rate` | float | Engine preemption rate (events/s, derived and normalized from the `*:num_preemptions_total` rate); under PagedAttention preemption means the whole request's KV is evicted and recomputed — the strongest overload negative-feedback signal; 0 for engines without this metric |
| `labels` | map[string]string | Backend custom labels |
| `raw` | map[string]float64 | All raw backend `/metrics` values; counters additionally expose `raw["rate:<metric name>"]` rate-derived values |
| `vars` | map[string]float64 | Variables injected by external Prometheus (indexed by `prometheus.queries[].name`, e.g. `vars["gpu_util"]`) |

expr built-in functions (`abs/ceil/floor/round/min/max`, etc.) can be used directly.
Adding a variable requires three things, in lockstep: registration in `scheduler.BuildEnv` + an update to this table + unit-test coverage.

## Interface (implemented in engine.go)

```go
func NewEngine() *Engine
func Validate(filter, score string) (normalizedFilter string, errs []ValidationError) // compile only, no effect
func (e *Engine) Set(name, filter, score string) error   // compile and register/hot-replace
func (e *Engine) Get(name string) *Policy
func (e *Engine) List() map[string]map[string]string     // source view (for admin)
func (p *Policy) Eval(env map[string]interface{}) (pass bool, score float64, err error)
```

## Built-in Presets (presets.go)

Common inference-scheduling objectives distilled into one-click switchable expression combinations, offered to the frontend as a dropdown:

| Preset | Use case | Core trade-off |
|---|---|---|
| `balanced` | General baseline | Balanced weighting of load / KV / prefix hits |
| `cache_affinity` | Multi-turn chats, RAG with a fixed system prompt | Prefix-hit weight ×2.5, maximizing prefix cache reuse |
| `latency_first` | Interactive chat/completion | `ttft_ewma` as the feedback loop; KV watermark tightened to 0.90, queue cap 32 — trades capacity for stable latency |
| `preemption_safe` | Long context, high-pressure scenarios | KV watermark tightened to 0.85 + heavy `preempt_rate` penalty + quadratic KV-occupancy penalty, avoiding preemption recompute |
| `throughput_first` | Offline batch / async jobs | Weakens the running-count weight (intra-batch parallelism is nearly free under continuous batching), favors backends with high `gen_tps` |

Presets only use variables precomputed on the Go side that are guaranteed to exist (no dynamic `raw`/`vars` keys), so they evaluate safely under any engine combination.

Usage:

- **Config reference**: `policies.<name>.preset: latency_first` (mutually exclusive with hand-written filter/score; expanded at load time);
- **One-click runtime switch**: `POST /admin/presets/{name}/apply` (optional `?policy=` selects the slot, default `default`), reusing the policy hot-reload channel — compile validation, persistence, and cluster broadcast all apply;
- **Frontend rendering**: `GET /admin/presets` returns the preset list (with Chinese titles and descriptions) and the currently active preset per policy slot; the active preset is derived by reverse-matching expressions (`MatchPreset`), hand-written expressions show as `custom` — no extra state to store, and all cluster instances agree naturally.

## Admin Editing and Debugging

- `POST /admin/policies/validate` accepts `{filter, score}`, performing only syntax and result-type validation; returns `valid` plus a per-`filter` / per-`score` error list. Nothing is registered, persisted, or changed at runtime. Because the compile environment permits unknown variables, this endpoint cannot guarantee that dynamic `vars[...]` / `raw[...]` keys exist at runtime.
- The console offers “visual builder / expression” dual modes. The builder covers common filters and linear weighted cost models, generating Expr live and calling the validation endpoint above; complex functions, parentheses, maps, and mixed logic stay in expression mode — no lossy reverse conversion.
- `GET /admin/explain?model=&prompt=&policy=` returns a per-backend score breakdown (ascending), answering “why was this request routed to X”. Score is cost: **lowest wins**.

## Files

| File | Description |
|---|---|
| `engine.go` | Compilation, registration, hot replacement, evaluation |
| `presets.go` | Built-in preset library and match/reverse-match |
| `engine_test.go` / `presets_test.go` | Compile/eval/filter/hot-update/error paths; all presets compile and evaluate |
