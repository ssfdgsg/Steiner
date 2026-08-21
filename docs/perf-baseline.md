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

## 管理面统计与大载荷转发（2026-08-10）

### 采集环境

- CPU: Apple M1（8 逻辑核）
- Go: 1.25.0（GVM）
- 管理面基准: `go test ./internal/metrics -run '^$' -bench 'BenchmarkAggregateHighCardinality|BenchmarkHistoryFullBufferSample' -benchmem -benchtime=2s -count=3`
- 大载荷测试: `go test ./internal/proxy -run 'TestLargeRequestBodyAndQueryForward|TestLargeNonStreamingResponse|TestLargeStreamingResponse' -count=3 -v`

### 管理面基准（三轮中位数）

| 场景 | ns/op 中位数 | 三轮范围 | B/op | allocs/op |
|---|---:|---:|---:|---:|
| 128 个后端序列 / 32 个模型聚合 | 615,472 | 612,578–616,757 | 686,791 | 13,710 |
| 1,440 点历史缓冲已满后采样 | 38,950 | 36,651–42,296 | 70,820 | 388 |

**解读**:

- 高基数聚合约 0.62ms/次。按单个控制台每秒轮询一次计算，CPU 成本很低，
  但会产生约 0.69MB/s 短命分配；若大量控制台或自动化客户端高频轮询，
  应增加缓存或降低轮询频率。
- 历史采样约 39µs/次，而生产采样间隔为 15s，CPU 占用可忽略；分配量折合
  约 4.7KB/s。

### 当前机器小请求转发上限（三轮中位数）

命令: `go test ./internal/proxy -run '^$' -bench '^BenchmarkForwardE2E$' -benchmem -benchtime=3s -count=3`

| 场景 | ns/op 中位数 | 三轮范围 | B/op 中位数 | allocs/op | 折算吞吐 |
|---|---:|---:|---:|---:|---:|
| 小型非流式补全，mock 上游，本机回环，8 路并行 | 34,449 | 34,207–34,848 | 66,065 | 285 | 约 29,000 QPS |

该数字是 Apple M1 上网关转发链路的实验室极限，不包含真实推理耗时、TLS、Ingress、
跨机网络与响应生成时间。生产规划不应按极限值满载；建议保留 30%–50% 余量，
并以生产规格压测结果为准。

### 大 query / response 完整性压力测试

大 query 快路径优化后的专项基准：

| 场景 | 三轮中位数 | B/op | allocs/op | 说明 |
|---|---:|---:|---:|---|
| 16MiB 完整 JSON + PromptText 解析 | 66.08ms | 33,563,818 | 29 | 需要完整 prompt 特征时的对照路径 |
| 16MiB 顶层元数据扫描 | 0.297ms | 453 | 5 | 只提取 model/stream/user/priority |
| 16MiB 端到端转发（本机 mock） | 7.55ms | 16,913,094 | 220 | 含精确 body 缓冲、选路和 HTTP 转发，约 132 QPS 串行 |

轻量元数据扫描相对完整解析约快 222 倍，并消除约 32MiB 的额外 prompt/doc
分配。端到端路径仍为重试保留一份请求体，不是真正的 socket 零拷贝；这是保留
失败换后端能力所需的有界成本。

| 场景 | 载荷 | 三轮耗时 | 完整性检查 | 结果 |
|---|---:|---:|---|---|
| 大 query | 16MiB JSON prompt + 128KiB URL query | 0.04–0.07s | 长度、Content-Length、SHA-256 | 通过 |
| 大非流式 response | 32MiB JSON | 0.04–0.05s | 长度、SHA-256、响应头、尾部 usage | 通过 |
| 大流式 response | 约 16MiB SSE | 0.02–0.03s | 长度、SHA-256、Flush、usage、`[DONE]` | 通过 |

三类测试连续执行三轮时，两次独立测量的测试进程峰值 RSS 为约 165.6–197.1MiB，
无截断、哈希不一致、错误响应或流式尾帧丢失。RSS 同时包含测试端预构造载荷、
上游测试服务、GC 时机和 `httptest.ResponseRecorder` 的全量响应缓存，波动较大；
生产容量评估应优先采用上表的每操作分配，并在真实进程中测 RSS。

### 生产适用性判断

结论为**有条件满足**，不能仅凭本机回环测试宣称完成生产容量认证：

- **管理面统计：满足。** 0.62ms 的高基数聚合和 39µs 的周期采样，明显低于
  秒级轮询和 15s 采样预算。
- **大 response 转发：满足传输层要求。** 网关使用 32KiB 缓冲逐块转发，只保留
  32KiB 尾部提取 usage，不会按响应总大小在网关内全量缓存；32MiB JSON 与约
  16MiB SSE 已验证逐字节完整。
- **大 query：普通无缓存路由已适合较高并发。** 无 KV、无 prompt 特征依赖、
  无 PD/模型改写时，只扫描顶层元数据并保留一份可重试 body；16MiB 实测约
  16.9MB/op。使用 KV、prompt 成本策略、PD、模型改写或无 session 的一致性哈希时
  仍会按需完整解码；这些路径应继续按请求体约 3 倍预留瞬时内存。
- **128KiB URL query：网关直连测试通过，但不代表完整生产链路通过。** Ingress、
  LB 或反向代理可能有独立的请求行/请求头限制；大输入应优先放在 request body，
  并在实际入口链路验证配置。

上线前仍应在与生产相同的容器内存限制、Ingress、TLS 和网络条件下，以目标并发
做 30–60 分钟 soak test，确认错误率为 0、RSS 峰值低于容器限制的 70%、无 OOM，
并记录网关增加的 p95/p99 时延与 GC CPU 占比。

## 重新采集

```bash
make bench          # 三组基准:调度选路 / 前缀树 / 转发链路
make stress         # 管理面竞态/基准 + 大 query / response 完整性压力测试
```

多实例行为的正确性由 `test/integration`(两套网关 + miniredis)覆盖,
随 `make test` 一起运行。
