# internal/kvcache/ — KV Cache 前缀感知路由

> 🌐 [English](README.en.md) | 简体中文

## 职责
在网关侧近似复刻各后端的 prefix cache 内容分布：维护一棵
「请求前缀 → 曾服务过该前缀的后端集合」压缩基数树（radix tree），
为调度提供逐后端前缀命中率（`prefix_match` 变量与 cache_aware 策略），
把携带相同长前缀的请求（多轮对话、固定 system prompt 的 RAG）尽量路由回同一后端，
命中其 RadixAttention / prefix cache，显著降低 prefill 开销与 TTFT。

## 原理与近似
- 不做真实 tokenize（避免引入各模型 tokenizer 依赖）：以 UTF-8 字节为边单位做
  压缩基数树匹配，与 sglang-router 的字符近似一致——字节前缀匹配与 token
  前缀高度相关，误差只影响收益期望，不影响正确性；
- 匹配文本 = 请求提示词拼接（chat: role+content 顺序拼接，多模态只取 text 分段；
  见 `internal/proxy/parse.go`），单条前缀纳入上限 `max_prefix_bytes`（默认 4KiB）；
- 树是**乐观近似**：后端自身也在淘汰 KV cache，匹配高≠必命中，
  因此不追求与后端严格同步。

## 实现（tree = radix.go）
- 节点带 `owners`（后端 ID → 最近访问时间）；Insert 沿途标记归属，
  Match 单次下行返回**每个后端**的最长命中字节数；
- 淘汰：TTL 过期（默认 10m）+ 周期清理（`RunPruner`，默认 30s），
  空叶子节点随之回收；规模统计（字节/节点数）暴露到自身指标与 `GET /admin/kvcache`；
- **内存硬上限（H8）**：`NewTreeWithBudget` 注入节点数（`max_nodes`，默认 10 万）
  与边字节数（`max_bytes`，默认 256MiB）双维度预算，任一超限时 Insert 按
  "最久未访问归属"逐代淘汰（复用 TTL 剪枝，每轮剔除全树最旧代际）并回收空节点，
  为新插入腾空间——异常/高基数输入下树规模有界，不依赖 TTL 兜底；
  `NewTree`（无预算）保持向后兼容；预算为 0 表示该维度不限；
- **后端摘除联动（L4）**：`RemoveBackedBy(backendID)` 批量清除树内该后端的
  死归属并回收节点；装配层经 `Registry.SetBackendRemovedHook` 在后端摘除
  （RemoveBackend 或 Upsert 替换）时调用，避免死归属残留至 TTL 过期、
  以及同 ID 重注册"继承"旧归属把亲和路由引向尚未持有 KV cache 的新实例；
- 并发：单互斥锁——树操作在微秒级、树规模受 TTL、前缀上限与内存预算约束，
  实测无需分片；如未来成为瓶颈，可按根首字节分 256 片（各分片天然独立）。

## 自研特例说明（按 CLAUDE.md 架构优先级要求留痕）
评估过 `armon/go-radix` 与 `hashicorp/go-immutable-radix`：均为通用 KV 语义，
不支持「节点多归属 + 逐归属时间戳 + 单次下行输出逐后端命中长度」，
改造成本高于收益，故按记录在案的特例实现专用树（参考 sglang-router tree 结构）。

## 文件
`radix.go`（树 + TTL 清理 + 统计）、`radix_test.go`（分裂/匹配/多归属/截断/过期用例）。
