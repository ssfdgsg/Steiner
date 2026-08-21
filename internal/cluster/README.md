# internal/cluster — 多实例协调层

> 🌐 [English](README.en.md) | 简体中文

以 Redis 为唯一协调存储，让多个网关实例可水平部署。`cluster.enabled=false`
（缺省）时本模块完全不参与，行为与单机模式一致。

## 能力

| 能力 | 机制 | 文件 |
|---|---|---|
| 实例注册与成员视图 | 心跳键 `{prefix}:instance:{id}`（TTL 自动过期），`GET /admin/cluster` 查询 | `cluster.go` |
| leader 选举 | `{prefix}:leader` 上的 NX+TTL 租约，Lua 原子续期；告警与扩缩容仅 leader 求值 | `cluster.go` |
| 分布式限流 | GCRA（`go-redis/redis_rate`），同一模型的全部实例共享 `rate_limit_qps` 配额 | `ratelimit.go` |
| 会话粘性共享 | 绑定表落 Redis（滑动 TTL），跨实例一致；本地表留热副本 | `session.go` |
| 策略热更新广播 | admin PUT 后经 pub/sub 频道 `{prefix}:policy` 同步全部实例 | `policy.go` |
| 后端变更广播 | admin 增删后端后经 `{prefix}:backends` 同步全部实例（持久化仅发起方执行） | `backends.go` |

## 降级原则

Redis 故障不影响数据面：

- 限流退回本地令牌桶（fail-open，总放行量临时变为 配额 × 实例数）；
- 会话粘性退回本地热副本（Bind 双写，Lookup 先 Redis 后本地）；
- 选举/广播/成员视图暂停，leader 判定维持故障前状态（租约同样无法被抢走）；
- 全部失败经 `gateway_cluster_redis_errors_total{op}` 计数，恢复后自动收敛。

启动期例外：`cluster.enabled=true` 但连不上 Redis 时直接启动失败（左移暴露配置
问题），而非带病运行。

## 各实例仍为本地的状态（设计取舍）

- **KV 前缀树**：每实例独立学习「前缀 → 后端」亲和。跨实例共享 radix 树的
  同步成本远超收益；后端真实的 prefix cache 命中率经指标直采已是全局信号，
  cache_aware 策略的失衡回退可自动纠偏。
- **inflight 计数**：本地值只反映本实例在途量；least_request/p2c 等策略主要
  依赖直采的 running/waiting（全局真值），本地 inflight 仅作次级信号。
- **熔断/健康状态**：各实例独立探测独立摘除，天然容忍单实例误判。

## 会话失效说明

后端健康翻转时 `session.Store.InvalidateBackend` 只清本地绑定；Redis 中的旧绑定
不主动清理——proxy 在使用绑定前必查 `backend.Available`，失效绑定会被跳过并在
下次成功转发后改绑，属于惰性收敛。
