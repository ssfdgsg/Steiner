# test/ — mock 后端与端到端验证

## 测试分层
| 层 | 位置 | 运行方式 |
|---|---|---|
| 单元/集成测试 | 各 `internal/*` 包内 `_test.go`（集成场景用 `test/mockbackend` 内嵌起真实 HTTP 后端） | `make test` |
| 冒烟 | `scripts/smoke.sh` 拉起 mock 后端 + 网关真实进程 | `make smoke` |

## 验收基线与对应用例
1. **基本转发**：非流式/流式补全、`X-Upstream-Backend` 头 → `internal/proxy/proxy_test.go`；
2. **指标驱动调度**：不同 running/waiting 下 least_request/expression 选择符合算式 →
   `internal/scheduler/scheduler_test.go`、`internal/metrics/scraper_test.go`；
3. **表达式热更**：合法更新生效、非法 400 且行为不变 → `internal/server/server_test.go`；
4. **前缀亲和**：同前缀收敛同后端、失衡回退 → `internal/scheduler/scheduler_test.go`、
   `internal/kvcache/radix_test.go`；
5. **故障处理**：拒连/503 重试换后端、4xx 透传不重试、连续失败熔断与恢复 →
   `internal/proxy/proxy_test.go`、`internal/backend/backend_test.go`；
6. **PD 分离**：两段式 kv_transfer_params 注入、bootstrap 双发一致性、
   NCCL 链路约束、prefill 故障重试 → `internal/proxy/pd_test.go`、`internal/pd/pd_test.go`；
7. **限流/排队/粘性/分流**：429、排队放行与超时、会话粘住与改绑、金丝雀分流 →
   `internal/proxy/{proxy,feature,route}_test.go`、`internal/queue`、`internal/session`；
8. **告警/扩缩容**：状态机、webhook 投递重试、建议冷却与钳位 → `internal/alerting/alerting_test.go`。

## 子目录
- `mockbackend/`：四引擎模拟器（指标渲染、SSE、故障注入、PD 行为模拟，见其 README）。
