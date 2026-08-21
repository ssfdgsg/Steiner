# internal/backend/ — Backend Abstraction and Registry

> 🌐 English | [简体中文](README.md)

## Responsibility

- `Backend`: a unified abstraction of a single inference backend instance; all runtime state (inflight, health, cordon, circuit breaking, metric snapshot, PromQL variables) is atomic, so the scheduling hot path reads it lock-free;
- `Registry`: built once at startup from config — the backend and model-route registry (incl. canary split sub-pools `Split`); read-only at runtime;
- Active health checks: periodically GET each backend's `health_path` (default `/health`); 2xx is healthy; status flips trigger callbacks (used to unbind sticky sessions and wake queued requests);
- Passive circuit breaking: when proxy-reported consecutive failures reach `failure_threshold`, the backend enters an `eject_cooldown` period and auto-recovers when it expires (no half-open probing — active health checks backstop it).

## Where Engine Differences Live

The four engines (vllm / vllm_omni / sglang / sglang_omni) differ in exactly two places; there is no per-engine subpackage:

- **Metric-name mapping**: the per-family mapping table in `internal/metrics/adapters.go` (omni variants reuse the base engine table via `EngineType.Family()`);
- **PD forwarding protocol**: `internal/proxy/pdproxy.go` selects two-phase (vllm) or bootstrap dual-send (sglang) by family.

## Core Interface (implemented in the respective files)

```go
func New(cfg config.BackendConfig) (*Backend, error)
func (b *Backend) Snapshot() *Snapshot            // lock-free read of the latest metric snapshot
func (b *Backend) TryAcquire() bool               // concurrency quota (max_concurrency)
func (b *Backend) Available(now time.Time) bool   // healthy && not cordoned && not in eject cooldown
func (b *Backend) MarkFailure(threshold int32, cooldown time.Duration)
func (b *Backend) MarkSuccess()

func NewRegistry(cfg *config.Config) (*Registry, error)
func (r *Registry) Route(model string) (*Route, error)  // falls back to "*" when no route matches
func NewHealthChecker(backends []*Backend, interval, timeout time.Duration,
    onChange func(*Backend, bool)) *HealthChecker
```

## Files

| File | Description |
|---|---|
| `backend.go` | Backend and all runtime state (incl. the Snapshot definition) |
| `registry.go` | Registry, model routes, split sub-pools |
| `health.go` | Active health-check loop |
| `backend_test.go` | Concurrency quota, circuit break/recovery, cordon cases |
