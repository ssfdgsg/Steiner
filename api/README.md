# api/ — 对外 HTTP 协议定义

网关单端口（配置 `server.listen`，默认 `:8080`）同时承载数据面与管理面；
实现位于 `internal/proxy`（数据面）与 `internal/server`（管理面与路由注册）。

## 数据面（OpenAI 兼容）

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | 聊天补全，支持 `stream=true` SSE；多模态 content parts 原样透传给 omni 后端 |
| `POST /v1/completions` | 文本补全 |
| `POST /v1/embeddings` 等其余 `/v1/*` | 按请求体 model 字段路由后原样透传 |
| `GET  /v1/models` | 返回配置的模型清单（兜底路由 `"*"` 不展示） |
| `GET  /healthz` | 网关存活探针（不依赖后端） |

转发语义：请求体透传，网关仅改写调度所需字段（`rewrite_model` 模型名映射、
PD bootstrap / kv_transfer_params 注入）；响应附加扩展头
`X-Upstream-Backend`（命中的后端 ID）。错误统一为 OpenAI 风格
`{"error":{"message","type","code"}}`（类型定义见 `pkg/openai`）。

会话粘性键：优先请求头 `X-Session-Id`，缺省回退请求体 `user` 字段。

## 管理面

| 端点 | 说明 |
|---|---|
| `GET  /metrics` | 网关自身 Prometheus 指标 + 后端归一化负载视图 |
| `GET  /admin/ui/` | **React 管理控制台**（构建产物嵌入二进制，无需额外部署静态服务）：调度方案一键切换、后端总览与增删隔离、调度解释器、运行态视图。`/admin/ui` 301 重定向到带斜杠形式 |
| `GET  /admin/backends` | 后端清单：健康/隔离/熔断状态、在途数、最近指标快照、PromQL 变量 |
| `POST /admin/backends` | 动态注册后端：body `{"id","url","engine","models":["路由名"],...}`；生效顺序 注册表 → 持久层（启用时，失败回滚）→ 集群广播；重复 id 409，PD 路由拒绝 |
| `DELETE /admin/backends/{id}` | 动态摘除后端（PD 组成员 409 拒绝）；在途请求正常完成，之后不再被选中 |
| `POST /admin/backends/{id}/cordon` | 人工隔离（不再接新请求） |
| `POST /admin/backends/{id}/uncordon` | 解除隔离 |
| `GET  /admin/policies` | 全部策略表达式源码 |
| `PUT  /admin/policies/{name}` | 热更新策略：body `{"filter":"...","score":"..."}`；编译成功才切换，失败 400 且运行策略不变 |
| `GET  /admin/presets` | 内置调度方案清单（name/title/description/filter/score）+ 各策略槽位当前生效方案（表达式反查，手写为 `custom`）——前端"一键切换"面板数据源 |
| `POST /admin/presets/{name}/apply` | 一键切换调度方案到策略槽位（可选 `?policy=`，默认 `default`）；复用热更通道（编译校验 → 持久化 → 集群广播），未知方案 404 |
| `GET  /admin/explain?model=&prompt=&policy=&session=` | 逐后端打分明细（调参与问题定位） |
| `GET  /admin/kvcache` | 前缀树规模统计 |
| `GET  /admin/pd` | PD 组拓扑与各 NCCL 链路在途传输数 |
| `GET  /admin/alerts` | 当前 pending/firing 告警（未启用时返回 enabled=false） |
| `GET  /admin/autoscale` | 各模型最近一次扩缩容建议（未启用时返回 enabled=false） |

## 状态码约定

| 码 | 场景 |
|---|---|
| 404 | 模型无路由且未配置兜底路由 `"*"` |
| 413 | 请求体超过 `server.max_body_bytes` |
| 429 | 模型级限流触发；容量排队队列已满 |
| 502 | 重试耗尽，全部后端转发失败 |
| 503 | 无可用后端；排队等待容量超时 |
