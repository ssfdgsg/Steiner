# deploy/grafana/ — 监控看板

## 文件
| 文件 | 说明 |
|---|---|
| `dashboard.json` | 网关总览看板（九面板）：请求 QPS（按后端）、错误率、TTFT P50/P95/P99、后端在途与健康、重试/限流/选路失败、KV 前缀树规模与排队深度、PD 链路在途传输、生成吞吐（token/s）、告警 firing 与扩缩容建议副本数 |

数据源为抓取了网关 `/metrics` 的 Prometheus（抓取配置见
`deploy/docker/prometheus.yml`）。导入方式：Grafana → Dashboards → Import →
粘贴 JSON。指标含义以 `internal/metrics/exporter.go` 与
`internal/metrics/collector.go` 为唯一权威来源。
