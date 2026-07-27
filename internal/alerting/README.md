# internal/alerting/ — 指标告警与自动扩缩容建议

## 职责
1. **webhook 通知器**（`notifier.go`）：异步队列投递、指数退避重试、五种消息模板（generic 原始 JSON / 钉钉 / 飞书 / 企业微信 / Slack）；
2. **告警规则引擎**（`rules.go`）：expr 布尔表达式周期求值，Prometheus 风格状态机
   `inactive → pending(持续 for) → firing → resolved`，跃迁与重复间隔时推送事件；
3. **自动扩缩容建议器**（`autoscale.go`）：按模型路由求值扩/缩表达式，产出期望副本数建议，经 webhook 推送给外部控制器（K8s operator / 运维机器人）落地。

## 关键设计
- **表达式与调度算式同一套变量**（`env.go`）：backend 作用域 = 调度表达式的后端侧变量（`running/waiting/kv_usage/hit_rate/gen_tps/raw/vars/...`，另加 `available/cordoned/ejected/scrape_err`）；cluster 作用域 = 按模型路由聚合（`avg_waiting/max_kv_usage/available_count/total_gen_tps/...`）。运维只需掌握一种方言；
- **网关只发信号、不执行扩缩容**：建议事件三路暴露——webhook（generic 模板给程序消费）、`gateway_autoscale_desired_replicas` 指标（可作 HPA/KEDA 外部指标源）、`Recommendations()` admin 视图。扩容出的新实例需加入网关配置并重载后才接流；
- **防震荡**：扩/缩各自独立冷却（默认扩 1m / 缩 5m，扩快缩慢），扩缩同时命中时扩容优先，期望副本数按 `min/max_replicas` 钳位；
- **失败隔离**：表达式求值失败按"不满足"处理只记日志；webhook 队列满丢弃不阻塞求值循环；规则/表达式编译失败在启动期直接失败（左移暴露）；
- 事件结构 `Event` 的 JSON 字段保持稳定，是对外契约（自动扩容控制器按此解析）。

## 自身指标（注册于 internal/metrics/exporter.go）
| 指标 | 说明 |
|---|---|
| `gateway_alerts_firing{rule}` | 每条规则当前 firing 实例数 |
| `gateway_webhook_sent_total{target,outcome}` | 投递结果（ok/failed/dropped） |
| `gateway_autoscale_desired_replicas{model}` | 建议副本数 |

## 配置
见 `configs/gateway.example.yaml` 的 `alerting` / `autoscale` 两段；结构体与校验在 `internal/config/config.go`。

## 文件清单
| 文件 | 说明 |
|---|---|
| `event.go` | 事件与建议明细结构（对外 JSON 契约） |
| `notifier.go` | webhook 异步投递、重试、模板渲染 |
| `env.go` | backend / cluster 两种作用域的求值环境 |
| `rules.go` | 规则编译、状态机、admin 活动告警视图 |
| `autoscale.go` | 扩缩容建议器、冷却与钳位、admin 建议视图 |
| `alerting_test.go` | 投递重试、模板、状态机全流程、集群聚合、扩缩建议与冷却、配置校验 |

## 待接入
- `internal/server` 暴露 `GET /admin/alerts`（`Engine.Active()`）与 `GET /admin/autoscale`（`Autoscaler.Recommendations()`）——引擎侧方法已就绪。
