# internal/alerting/ — Metric Alerts and Autoscale Recommendations

> 🌐 English | [简体中文](README.md)

## Responsibility

1. **Webhook notifier** (`notifier.go`): async queue delivery, exponential backoff retry, five message templates (generic raw JSON / DingTalk / Feishu / WeCom / Slack);
2. **Alert rule engine** (`rules.go`): periodic evaluation of expr boolean expressions, Prometheus-style state machine `inactive → pending(held for for) → firing → resolved`; events are pushed on transitions and on the repeat interval;
3. **Autoscale recommender** (`autoscale.go`): evaluates scale-out/scale-in expressions per model route, produces desired-replica recommendations, and pushes them via webhook to external controllers (K8s operator / ops bot) to act on.

## Key Design Points

- **Same expression variables as scheduling formulas** (`env.go`): the backend scope = the scheduling expression's backend-side variables (`running/waiting/kv_usage/hit_rate/gen_tps/raw/vars/...`, plus `available/cordoned/ejected/scrape_err`); the cluster scope = aggregation per model route (`avg_waiting/max_kv_usage/available_count/total_gen_tps/...`). Ops only need to learn one dialect;
- **The gateway only emits signals, it does not execute scaling**: recommendation events are exposed three ways — webhook (generic template for programmatic consumption), the `gateway_autoscale_desired_replicas` metric (usable as an HPA/KEDA external metric source), and the `Recommendations()` admin view. Newly scaled instances only start receiving traffic after they are added to the gateway config and reloaded;
- **Anti-oscillation**: scale-out and scale-in have independent cooldowns (defaults: 1m out / 5m in — scale fast, shrink slow); when both fire at once, scale-out wins; the desired replica count is clamped to `min/max_replicas`;
- **Failure isolation**: expression evaluation failures are treated as “not satisfied” and only logged; a full webhook queue drops messages without blocking the evaluation loop; rule/expression compile failures fail startup outright (shift-left exposure);
- The `Event` structure's JSON fields are stable and are an external contract (autoscale controllers parse it).

## Self Metrics (registered in internal/metrics/exporter.go)

| Metric | Description |
|---|---|
| `gateway_alerts_firing{rule}` | Current firing instance count per rule |
| `gateway_webhook_sent_total{target,outcome}` | Delivery outcomes (ok/failed/dropped) |
| `gateway_autoscale_desired_replicas{model}` | Recommended replica count |

## Configuration

See the `alerting` / `autoscale` sections of `configs/gateway.example.yaml`; structs and validation live in `internal/config/config.go`.

## File List

| File | Description |
|---|---|
| `event.go` | Event and recommendation-detail structures (external JSON contract) |
| `notifier.go` | Async webhook delivery, retry, template rendering |
| `env.go` | Evaluation environments for backend and cluster scopes |
| `rules.go` | Rule compilation, state machine, admin active-alerts view |
| `autoscale.go` | Autoscale recommender, cooldowns and clamping, admin recommendations view |
| `alerting_test.go` | Delivery retry, templates, full state machine, cluster aggregation, scale recommendations and cooldowns, config validation |

## To Be Wired

- `internal/server` exposes `GET /admin/alerts` (`Engine.Active()`) and `GET /admin/autoscale` (`Autoscaler.Recommendations()`) — the engine-side methods are ready.
