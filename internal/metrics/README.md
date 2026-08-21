# internal/metrics/ — 指标子系统（双通道 + 自身导出）

> 🌐 [English](README.en.md) | 简体中文

## 职责
```
后端 /metrics ──(Scraper, 秒级)──▶ backend.Snapshot（原子指针发布）─▶ 调度热路径无锁读
Prometheus  ──(PromCollector)──▶ backend.PromVars（表达式 vars["..."]）─┘
网关自身    ──(Gateway 指标集 + BackendCollector)──▶ GET /metrics
```

两通道定位：直采负责**低延迟核心信号**（running/waiting/kv 占用，秒级新鲜度）；
PromQL 负责**引擎暴露不了的外部信号**（DCGM GPU 利用率、温度、网络带宽等）。

## 直采（scraper.go + adapters.go）
- `prometheus/common/expfmt` 官方解析器解析后端 exposition 文本；
- 同名多序列（多模型标签）求和；counter 做窗口差分产出 `rate:<指标名>` 派生值；
- 归一化字段由 `adapters.go` 的按引擎家族候选名表选取（新老版本指标名并存，
  按序取第一个命中）；命中率在 gauge 缺失时由 hits/queries counter 对的速率比推导；
- 采集失败保留旧值、只追加 `Err` 标记，避免指标闪断引起调度抖动。

归一化字段：`Running` / `Waiting` / `KVUsage` / `HitRate` / `GenTokPerSec` / `Raw`（全量）。
指标名映射表（唯一权威来源为 `adapters.go` 源码）：

| 字段 | vllm 家族候选 | sglang 家族候选 |
|---|---|---|
| Running | `vllm:num_requests_running` | `sglang:num_running_reqs` |
| Waiting | `vllm:num_requests_waiting` | `sglang:num_queue_reqs` |
| KVUsage | `vllm:kv_cache_usage_perc`, `vllm:gpu_cache_usage_perc` | `sglang:token_usage`, `sglang:kv_cache_usage` |
| HitRate | `vllm:gpu_prefix_cache_hit_rate`；缺失时 rate(hits)/rate(queries) | `sglang:cache_hit_rate` |
| GenTokPerSec | rate(`vllm:generation_tokens_total`) | `sglang:gen_throughput`（gauge 直读） |

## PromQL 旁路（promql.go）
- 官方 `client_golang/api/v1` 客户端周期执行配置查询（即时向量）；
- 结果按 `backend_label` 标签值匹配后端（优先后端 `labels` 同名项，其次 URL host）；
- 每轮整体重建变量表，下线序列不残留旧值。

## 自身指标（exporter.go + collector.go，本表为唯一权威来源）
全部注册在独立 Registry，`GET /metrics` 导出。

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `gateway_requests_total` | counter | backend,model,code | 转发请求总数 |
| `gateway_request_duration_seconds` | histogram | backend,model | 整请求时延 |
| `gateway_time_to_first_byte_seconds` | histogram | backend,model | 首字节时延（流式即 TTFT） |
| `gateway_retries_total` | counter | — | 换后端重试次数 |
| `gateway_rate_limited_total` | counter | model | 模型级限流拒绝数 |
| `gateway_pick_errors_total` | counter | model,reason | 选路失败（no_route/no_backend/queue_full/queue_timeout/exhausted/pd_*） |
| `gateway_pick_duration_seconds` | histogram | strategy | 单次选路耗时 |
| `gateway_upstream_errors_total` | counter | backend,kind | 上游错误分类（connect/bad_status/stream） |
| `gateway_prompt_tokens_total` / `gateway_completion_tokens_total` | counter | backend,model | 上游 usage token 用量（流式取末块） |
| `gateway_backend_healthy` / `gateway_backend_inflight` | gauge | backend | 健康状态 / 网关侧在途（5s 周期刷新） |
| `gateway_kvtree_bytes` / `gateway_kvtree_nodes` | gauge | — | 前缀树规模 |
| `gateway_queue_depth` | gauge | model | 排队深度（抓取瞬间实时读，按路由分列） |
| `gateway_pd_link_inflight` | gauge | prefill,decode | PD 链路在途 KV 传输数 |
| `gateway_split_requests_total` | counter | model,split | 金丝雀分流命中计数 |
| `gateway_backend_info` | gauge | backend,engine,url | 后端元信息（恒 1） |
| `gateway_backend_running_requests` / `_waiting_requests` | gauge | backend | 引擎负载归一化视图 |
| `gateway_backend_kv_cache_usage` / `_prefix_hit_rate` / `_gen_tokens_per_second` | gauge | backend | KV 占用 / 前缀命中率 / 生成吞吐 |
| `gateway_backend_scrape_up` | gauge | backend | 最近一次直采是否成功 |
| `gateway_alerts_firing` | gauge | rule | 每规则 firing 实例数 |
| `gateway_webhook_sent_total` | counter | target,outcome | webhook 投递结果 |
| `gateway_autoscale_desired_replicas` | gauge | model | 扩缩容建议副本数（可作 HPA/KEDA 外部指标） |
| `gateway_build_info` | gauge | version | 构建信息（恒 1） |

（集群模式另有 `gateway_cluster_*` 指标，见 exporter.go。）
归一化视图（`gateway_backend_*`）由自定义 Collector 在抓取瞬间实时读原子快照，
Prometheus 只抓网关即可获得全集群引擎负载，且后端摘除后不留陈旧序列。

## 管理面聚合与时序（stats.go + history.go）
控制台需要「当前吞吐是多少、时延分位数如何、最近几小时的趋势」，但控制台不应被迫
依赖外部 Prometheus 才能显示基础 KPI。因此：

- `stats.go` 的 `Gateway.Aggregate()` 直接 `Registry.Gather()` 汇总——**与 `/metrics`
  同源同口径**，不新增运行期计数状态。分位数由直方图累计桶线性插值估算，精度受桶边界
  限制，用于趋势判断而非 SLA 计算；`code >= 400` 与非数字码计为错误。
- `history.go` 的 `History` 是进程内环形缓冲（默认 15s × 1440 ≈ 6 小时，见
  `cmd/gateway/main.go` 常量），周期采样并对 counter 做差分得到瞬时 RPS / 错误率 /
  区间平均时延；排队深度、可用实例数、KV 规模等不在自身指标里的运行态由装配期注入的
  `RuntimeProbe` 同点采集，保证各维度时间对齐。首个采样点仅作基线不入缓冲。
- 边界：缓冲不持久化、不跨实例聚合，重启即清空。长周期、高保真、跨副本的时序仍以
  Prometheus 为事实来源，控制台在图表旁保留 `/metrics` 入口。

## 文件
| 文件 | 说明 |
|---|---|
| `adapters.go` | 引擎家族 -> 归一化字段的指标名映射表 |
| `scraper.go` | 直采循环（expfmt 解析、求和、差分、命中率推导） |
| `promql.go` | 外部 Prometheus 周期查询与变量注入 |
| `exporter.go` | 网关自身指标集 |
| `collector.go` | 后端归一化视图 / 路由分流计数透出 |
| `stats.go` | 管理面聚合统计（Registry 汇总、直方图分位数估算） |
| `history.go` | 管理面时序环形缓冲与运行态探针 |
| `scraper_test.go` | 双引擎解析、速率派生、失败保旧值、counter 命中率用例 |
| `stats_test.go` | 聚合口径、差分语义与缓冲淘汰用例 |
