# ADR-0001：技术选型

- 状态：已接受
- 日期：2026-07-26

## 背景
网关核心诉求：策略表达式动态编译、双通道指标接入、SSE 反向代理、前缀感知路由。需在「生态复用优先、禁止无谓自研」的约束下确定依赖面。

## 决策
| 能力 | 选型 |
|---|---|
| 表达式引擎 | `github.com/expr-lang/expr` |
| 指标解析/查询/暴露 | `prometheus/common(expfmt)`、`prometheus/client_golang(api/v1 与 SDK)` |
| 限流 | `golang.org/x/time/rate` |
| 配置 | `gopkg.in/yaml.v3` |
| HTTP 路由与反代 | 标准库 `net/http`（1.22 方法路由）+ `httputil.ReverseProxy` |
| 前缀树 | 专用自研（记录在案的特例，见下） |

## 备选与否决理由
- **表达式**：`govaluate`（停止维护、无类型检查）、`cel-go`（能力足但为协议校验场景设计，数值调度算式表达繁琐、依赖重）、Lua/Wasm 嵌入（功率过剩，冷启动与沙箱成本高）→ 否决；expr 编译期类型检查 + 环境白名单 + 微秒级求值最匹配；
- **web 框架**（gin/chi/echo）：网关只有个位数路由，标准库 1.22 方法路由已够，少一层依赖与升级面 → 否决引入；
- **反代自写 io.Copy 循环**：`ReverseProxy` 已处理 hop-by-hop 头、flush、升级协议等边角 → 否决自写；
- **前缀树复用**（`armon/go-radix`、`hashicorp/go-immutable-radix`）：通用 KV 语义，无法承载「边按字符块切分、节点多后端 LRU 元数据、按全局字节数淘汰」需求，包一层的改造代码量接近直接实现，且热路径多一次接口间接 → **批准自研特例**，范围严格限定在 `internal/kvcache`，对齐 sglang-router `tree.rs` 的成熟结构以降低设计风险。

## 后果
- 正面：依赖面小（6 个直接依赖），全部为官方或事实标准库；表达式热更与无锁快照满足性能目标；
- 负面：自研树需要额外的模糊测试与基准投入（已列入 `internal/kvcache` 测试规划）；标准库路由不支持复杂通配，未来若管理面膨胀可能需要引入 chi（届时增补 ADR）。
