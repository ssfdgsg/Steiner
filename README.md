# Steiner — LLM 推理负载均衡与调度网关

> 🌐 [English](README.en.md) | 简体中文

> 面向 vLLM 与 SGLang 的 OpenAI 兼容七层网关：以指标驱动、缓存感知的调度提升 LLM 推理集群的吞吐与稳定性。

Steiner 将多种推理引擎收敛为统一的 OpenAI 兼容入口，并提供模型级路由、负载均衡、KV Cache 前缀亲和、PD 分离、可观测性与多实例协调能力。它适合需要在 Kubernetes 或自建集群中统一接入和调度大模型推理服务的团队。

**许可证：** [Apache-2.0](LICENSE) · **贡献指南：** [CONTRIBUTING.md](CONTRIBUTING.md) · **安全报告：** [SECURITY.md](SECURITY.md)

## 快速开始

前置条件：Go 1.22；如需修改管理控制台，还需要 Node.js LTS。

```bash
make test
make build
make smoke
```

使用 Docker 启动包含两个 mock 后端和 Prometheus 的本地演示环境：

```bash
make up
# 控制台：http://localhost:8080/admin/ui/
```

停止演示环境：`make down`。

面向 **vLLM / vLLM-Omni / SGLang / SGLang-Omni** 四类推理引擎的七层网关，提供：

- **多引擎负载均衡**：统一 OpenAI 兼容入口，按引擎适配器屏蔽协议与指标差异；模型级路由（每模型独立后端池/策略/限流）、模型名改写（对外统一名 → 后端部署名）、权重分池金丝雀分流；
- **指标驱动调度**：双通道指标接入（直采后端 `/metrics` + 远程 Prometheus PromQL），秒级刷新指标快照；
- **策略表达式动态编译**：调度算式以表达式（expr-lang/expr）书写，运行期编译为字节码、原子热切换，无需重启即可变更调度逻辑；
- **KV Cache 前缀感知路由**：近似基数树（radix tree）维护「前缀 → 后端」亲和，最大化 prefix cache 命中，失衡时自动回退到负载优先；
- **PD 分离与 NCCL/NIXL 连接组调度**：prefill / decode 角色化实例配对，仅在已建立 KV 传输通道（NCCL / NIXL / Mooncake）的 P-D 对之间派发请求；
- **webhook 告警与自动扩缩容信号**：告警规则（与调度算式同一套表达式变量）周期求值，pending/firing/resolved 状态机推送钉钉/飞书/企业微信/Slack/generic webhook；扩缩容建议器产出期望副本数，供外部控制器（K8s operator / HPA / KEDA）落地扩容；
- **多实例水平部署**：以 Redis 为协调层——分布式限流（GCRA，全集群共享模型配额）、会话粘性共享、策略热更新广播、告警/扩缩容 leader 单主执行、`/admin/cluster` 成员视图；Redis 故障自动降级回本地行为（**可用性取舍**：fail-open 下降级为每实例本地桶，故障期间集群配额可超至 N×，配置 `rate_limit_fail_open: false` 可改为故障时拒绝以严格限额）；
- **分布式追踪（OpenTelemetry）**：每请求一条 span 链（排队等待 / 每次选路 / 每次重试转发 / TTFT 事件 / token 用量），OTLP 导出，`traceparent` 注入上游可与引擎侧续接全链路；`X-Trace-Id` / `X-Request-Id` 响应头与日志、指标互查；
- **动态后端注册与配置持久化**：`POST/DELETE /admin/backends` 运行期增删后端（池 copy-on-write 无锁热切换），变更经 PG/MySQL 持久层落库、重启自动恢复，并经集群广播同步全部实例——与扩缩容建议器闭环：外部控制器扩容后调 admin API 即可接流，无需重启；
- **React 管理控制台**：`/admin/ui/` 开箱即用（产物嵌入二进制，无需部署静态服务；静态壳公开加载，进入后需输入 `server.admin_token` 登录，令牌仅存于浏览器 localStorage）——调度方案一键切换、后端实时负载与增删隔离、调度解释器（回答"为什么路由到 X"）、KV/排队/PD 链路/告警/扩缩容/集群运行态视图；
- **流式透传、令牌桶限流、会话粘性、熔断摘除、自身可观测性**。

## 总体架构

```
                          ┌────────────────────────────────────────────────────┐
                          │                      Steiner                       │
  客户端                   │                                                    │
  (OpenAI SDK) ──────────▶│  server ──▶ proxy(特征提取/限流) ──▶ scheduler     │
  /v1/chat/completions    │   /v1/*       │                       │   ▲        │
  /v1/completions         │               │        ┌──────────────┘   │        │
                          │               │        ▼                  │        │
                          │               │  8 种内置策略             │        │
                          │               │  policy(表达式VM/热切换)  │        │
                          │               │  kvcache(radix 前缀树)    │        │
                          │               │  session(一致性哈希环)    │        │
                          │               │  pd(P-D 连接组)           │        │
                          │               │            │              │        │
                          │               │            ▼              │        │
                          │               │      backend 注册表 ──────┘        │
                          │               │      (健康检查/熔断/cordon)        │
                          │               │            ▲                       │
                          │               │   metrics(直采+PromQL+指标名适配)  │
                          │               ▼                                    │
                          │    proxy 转发(SSE 透传/TTFT/首字节前重试/PD 两段式)│
                          │                                                    │
                          │  admin API(策略热更新/cordon/explain/kvcache/pd)   │
                          │  alerting(告警状态机/扩缩容建议) ──▶ webhook 通知  │
                          │  cluster(选举/分布式限流/会话共享) ◀─▶ Redis       │
                          │  store(动态后端/策略持久化) ◀─▶ PostgreSQL/MySQL   │
                          └───────────────┬────────────────┬───────────────────┘
                                          ▼                ▼
                               vLLM / vLLM-Omni      SGLang / SGLang-Omni
                               (含 PD 分离部署：prefill 池 + decode 池)
```

## 请求生命周期

1. **接入**（`internal/server`）：解析路由，读取请求体（受 `max_body_bytes` 约束）；
2. **特征提取**（`internal/proxy/parse.go`）：模型名、是否流式、prompt 文本（用于前缀匹配，多模态取 text 分段）、会话键、优先级；
3. **准入**（`internal/proxy`）：模型级令牌桶限流超限快速失败；无可用容量时经 `internal/queue` 短暂排队（队列满 429 / 超时 503）；
4. **候选过滤**：backend 注册表按「健康 && 支持该模型 && 未摘除 && PD 角色匹配」给出候选集；
5. **打分选择**（`internal/scheduler`）：会话粘性命中则短路；否则由当前策略（内置策略或表达式策略）对候选逐一求值，结合 `internal/kvcache` 前缀匹配率与 `internal/pd` 连接组约束，选出目标后端（PD 模式下为 P-D 对）；
6. **转发**（`internal/proxy`）：改写请求（模型名映射、PD bootstrap 注入），SSE 流式透传；首字节前失败可重试换后端；
7. **回写**：更新 radix tree、inflight 计数、网关自身指标（`internal/metrics`）。

## 目录导航

| 路径 | 职责（详见各目录 README） |
|---|---|
| `cmd/gateway/` | 进程入口：装配各模块、启动 HTTP 服务与后台循环 |
| `configs/` | YAML 配置示例与配置项完整说明 |
| `api/` | 对外 HTTP 协议：数据面（OpenAI 兼容）与管理面（admin API）定义 |
| `internal/config/` | 配置加载、校验、热更新分发 |
| `internal/server/` | HTTP 路由、中间件、admin API、优雅退出 |
| `internal/proxy/` | 反向代理、SSE 流式、重试与请求改写 |
| `internal/backend/` | 后端抽象、注册表（含分流子池）、健康检查、熔断摘除（引擎差异集中在 metrics 映射表与 proxy PD 协议） |
| `internal/metrics/` | 指标子系统：/metrics 直采、PromQL 远程查询、统一快照、自身指标权威表 |
| `internal/policy/` | 策略表达式引擎：编译、热切换、变量表（唯一权威来源） |
| `internal/scheduler/` | 调度核心：八种内置策略与打分解释 |
| `internal/kvcache/` | 前缀感知路由：radix tree、亲和、淘汰、失衡回退 |
| `internal/pd/` | PD 分离编排与 NCCL/NIXL/Mooncake 连接组管理 |
| `internal/alerting/` | webhook 告警（规则状态机 + 多模板通知）与自动扩缩容建议 |
| `internal/session/` | 会话粘性（提高多轮对话 KV 命中） |
| `internal/cluster/` | 多实例协调层：leader 选举、分布式限流、会话共享、策略/后端变更广播（Redis） |
| `internal/store/` | 动态配置持久层：后端/策略落库与启动恢复（PostgreSQL / MySQL） |
| `internal/queue/` | 容量排队（事件驱动唤醒 + 兜底轮询） |
| `internal/tracing/` | OpenTelemetry 初始化：OTLP 导出、采样、W3C 传播（打点在 proxy） |
| `web/` | React 管理控制台源码（Vite + TS，构建产物嵌入二进制） |
| `internal/webui/` | 控制台产物嵌入与静态托管（`/admin/ui/`） |
| `pkg/openai/` | OpenAI 协议类型（可被外部工具复用；限流在 proxy，自身指标在 metrics） |
| `test/` | 集成测试与四引擎 mock 后端 |
| `deploy/` | Docker Compose / K8s / Grafana 部署物 |
| `docs/` | 架构决策记录（ADR） |
| `scripts/` | 本地验证脚本（构建、测试、冒烟） |

## 技术选型（生态复用优先）

| 能力 | 选型 | 理由 |
|---|---|---|
| 表达式引擎 | `expr-lang/expr` | 社区事实标准：编译为字节码、类型检查、可设超时与环境白名单，满足"动态编译算式"且安全可控 |
| 指标文本解析 | `prometheus/common/expfmt` | 官方 exposition 格式解析器，直采后端 `/metrics` 零自研 |
| PromQL 查询 | `prometheus/client_golang/api/v1` | 官方 HTTP API 客户端 |
| 自身指标 | `prometheus/client_golang` | 官方 SDK |
| 配置 | `gopkg.in/yaml.v3` | 事实标准 |
| 集群协调 | `redis/go-redis` + `go-redis/redis_rate`（GCRA 限流）| 官方客户端与作者同源限流库；测试用 `miniredis` 本地可验证 |
| 配置持久层 | 标准库 `database/sql` + `jackc/pgx`（PG）/ `go-sql-driver/mysql` | 两家官方推荐驱动；方言差异集中一处，测试用 `sqlmock` 本地可验证 |
| HTTP 路由/代理 | 标准库 `net/http`（1.22 方法路由；转发手写实现以支持重试换后端 / PD 两段式 / TTFT 打点） | 零框架依赖 |
| 基数树 | 优先评估 `armon/go-radix`；淘汰/多租户元数据不满足时按已记录特例自研（见 `internal/kvcache/README.md`） | — |

## 生态参考实现

- **sglang-router**：cache-aware 路由（近似 radix tree + 绝对/相对失衡阈值回退）与 PD 路由（请求同时派发 P/D、携带 bootstrap 信息）；
- **vLLM production-stack router**：会话粘性、KV 感知路由；
- **AIBrix Gateway / Envoy AI Gateway**：网关层限流、模型路由形态。

## 构建与验证

```bash
make build      # 编译（GOTOOLCHAIN=local）
make test       # 单元测试 + 集成测试（test/integration：双网关 + miniredis）
make smoke      # 启动 mock 后端 + 网关的本地冒烟
make web        # 构建 React 控制台（改前端时才需要；dist 已提交，go build 不依赖 Node）
make web-dev    # 前端热更开发（Vite :5173 代理到本地网关 :8080）
make bench      # 性能基线：调度选路/前缀树/转发链路（基线见 docs/perf-baseline.md）
make conformance # 真实引擎验证：docker 起官方 vLLM CPU 镜像 + 小模型，
                 # 验证指标名适配/SSE 分帧/usage 提取/模型名改写/健康检查
                 # （首次运行需下载镜像与模型；KEEP_UP=1 可保留环境调试）
```

> 本机注意事项：WSL2 下 `GOTOOLCHAIN` 需固定为 `local`（go 1.22.2），Makefile 已内置，避免触发工具链自动下载卡死。
