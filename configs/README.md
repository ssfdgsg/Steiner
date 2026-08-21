# configs/ — 配置文件

## 职责
存放网关的 YAML 配置样例。运行期由 `internal/config` 加载与校验；
本目录只放**样例与文档**，不放环境私有配置。

## 文件
| 文件 | 说明 |
|---|---|
| `gateway.yaml` | 可直接运行的参考配置（`-config` 默认路径） |
| `gateway.example.yaml` | 全量注释示例，覆盖所有配置段，可复制裁剪 |

## 配置段总览（与 internal/config/config.go 结构体一一对应）
- `server`：监听地址、请求体上限、重试次数、上游超时、熔断阈值与冷却期；
- `metrics`：后端 /metrics 直采与主动健康检查的周期/超时；
- `prometheus`：外部 PromQL 通道（地址、查询列表、标签映射）；
- `kv_cache`：前缀感知路由（前缀上限、TTL、亲和阈值、失衡双阈值）；
- `policies`：命名策略表达式（filter/score，变量表见 `internal/policy/README.md`）；
- `backends[]`：后端实例（引擎类型、地址、权重、并发上限、标签、bootstrap 端口）；
- `models[]`：模型路由（backends / splits 金丝雀分流 / pd_group 三选一，
  策略、限流、模型名改写）；
- `pd_groups[]`：PD 分离组（prefill/decode 池、nccl_links 链路声明）；
- `session` / `queue`：会话粘性、容量排队；
- `alerting` / `autoscale`：告警规则与 webhook、自动扩缩容建议。

## 约定
- 时长字段支持 Go duration 字符串（`500ms`、`2s`）或纯数字（按秒解释）；
- 策略表达式运行期可经 `PUT /admin/policies/{name}` 热更新，其余配置段重启生效。

## 安全（C1/H1 修复后）
- `server.admin_token` **必填**：管理面 `/admin/*` 全部要求
  `Authorization: Bearer <token>`，未配置时网关拒绝启动；
- 默认监听收紧为 `127.0.0.1:8080`（仅本机回环），对外暴露需显式配置
  `listen: "0.0.0.0:8080"`；
- 管理面变更类请求（POST/PUT/PATCH/DELETE）强制 `Content-Type: application/json`，
  非 JSON 一律 415（切断跨站表单投递的 CSRF 前置）。
