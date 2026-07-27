# 性能基线

记录关键热路径的基准数据,调度器/代理/前缀树改动后运行 `make bench` 对照,
显著回退(>20%)应在提交说明中解释原因或先行优化。

## 采集环境

- 日期:2026-07-26
- CPU:13th Gen Intel Core i7-13700HX(WSL2,-8 并行度)
- Go:1.22.2(GOTOOLCHAIN=local)
- 命令:`make bench`

## 调度选路(16 后端池,单次 Pick)

| 策略 | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| round_robin | 109 | 128 | 1 |
| random | 110 | 128 | 1 |
| weighted_random | 143 | 128 | 1 |
| least_request | 150 | 128 | 1 |
| p2c | 146 | 128 | 1 |
| consistent_hash | 902 | 1,092 | 4 |
| cache_aware | 3,331 | 3,387 | 9 |
| expression | 34,645 | 29,154 | 403 |

**解读**:

- 简单策略在 100~150ns 量级,对毫秒级请求完全可忽略;
- cache_aware 的开销主要在前缀树 Match(见下),3.3µs 仍可忽略;
- **expression 是最大的优化候选**:每次选路对每个候选后端跑一遍 expr VM,
  且每次求值都重建环境 map(403 次分配大头在此)。优化方向:环境 map 复用
  (sync.Pool)或按快照代次缓存打分结果。当前绝对值(35µs)相对推理时延
  (百 ms~s)仍可忽略,暂不阻塞。

## KV 前缀树(1024 条记录)

| 操作 | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Insert | 4,933 | 616 | 8 |
| Match | 1,943 | 1,734 | 6 |

## 转发链路端到端(mock 后端,含本机回环 HTTP 往返)

| 场景 | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| 非流式补全(least_request) | 49,866 | 59,644 | 245 |

**解读**:单请求网关自身开销上界约 50µs(含 mock 后端往返与 httptest 开销),
折合单核约 2 万 QPS;真实部署中 GPU 推理时延为百 ms~s 量级,网关开销占比
低于千分之一,不构成瓶颈。

## 重新采集

```bash
make bench          # 三组基准:调度选路 / 前缀树 / 转发链路
```

多实例行为的正确性由 `test/integration`(两套网关 + miniredis)覆盖,
随 `make test` 一起运行。
