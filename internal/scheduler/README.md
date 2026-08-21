# internal/scheduler/ — 调度核心

> 🌐 [English](README.en.md) | 简体中文

## 职责
给定请求特征与模型路由，从候选后端中选出转发目标。所有策略共享同一候选过滤：
健康、未人工隔离、不在熔断冷却期、未被本次重试排除、并发额度未占满。

## 八种策略（`Pick` / `PickAmong` 分发，实现见 scheduler.go / ring.go）

| 策略 | 语义 |
|---|---|
| `round_robin` | 轮询 |
| `random` | 随机 |
| `weighted_random` | 按静态权重随机 |
| `least_request` | 最小综合负载（网关在途 + 引擎运行 + 引擎排队） |
| `p2c` | 两次随机取负载较小者（Power of Two Choices） |
| `consistent_hash` | 一致性哈希（键：会话 ID > 提示词 > 模型名；64 虚拟节点/后端，不可用时顺时针转移） |
| `cache_aware` | KV 前缀感知：最高命中率 ≥ `match_threshold` 且目标未失衡则亲和路由，否则回退最小负载（失衡判定为 abs+rel 双阈值，对齐 sglang-router） |
| `expression` | 策略表达式打分（policy 引擎动态编译，分数最小者胜出；全部被过滤时降级最小负载） |

## 与各模块的协作
- **kvcache**：`prefixMatches` 计算逐后端前缀命中率，作为 `prefix_match` 表达式变量
  与 cache_aware 决策依据；转发成功后由 proxy 调 `Observe` 回写前缀归属；
- **policy**：`BuildEnv` 构建求值环境（变量表唯一权威来源：`internal/policy/README.md`）；
- **pd**：PD 组的 prefill 侧复用 `PickAmong`（全部策略可用）；
- **会话粘性**：由 proxy 在调度前短路（`internal/session`），本包不感知；
- **金丝雀分流**：路由带 splits 时先按权重选子池再在子池内选路（可用性优先于分流比例）。

## 可解释性
`Explain(route, policy, req)` 返回逐后端打分明细（升序），由
`GET /admin/explain` 暴露。

## 文件
| 文件 | 说明 |
|---|---|
| `scheduler.go` | 策略分发、候选过滤、表达式环境、Explain |
| `ring.go` | 一致性哈希环（按候选集合签名缓存） |
| `scheduler_test.go` | 全部策略与边界条件用例 |
