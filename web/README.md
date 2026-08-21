# web/ — React 管理控制台

网关的可视化管理界面。构建产物输出到 `internal/webui/dist/`，由 `go:embed`
打进网关二进制，运维**无需部署额外的静态服务器**，启动网关后访问
`http://<网关地址>/admin/ui/` 即可。

## 技术选型
| 能力 | 选型 | 理由 |
|---|---|---|
| 框架 | React 18 + TypeScript | 需求明确指定 React；TS 让 admin API 响应结构有编译期约束 |
| 构建 | Vite 5 | 零配置起步、构建快、产物小（gzip 约 67 KB） |
| 路由 | hash 路由（`#/overview`） | 嵌入式静态托管无需服务端改写规则，刷新不 404 |
| 状态/请求 | 原生 `fetch` + 自研 `usePoll` | 只有轮询只读接口与少量写操作，引入状态库得不偿失 |
| 样式 | 原生 CSS 变量（暗色优先） | 语义化 token 统一多页面视觉；无 CDN 依赖，兼容内网隔离环境 |
| 图标 / 图表 | 内联 SVG（`icons.tsx` / `charts.tsx`） | 只需十余个图标与三种图形（折线/柱状/迷你趋势），内联实现体积为零且可审计 |

刻意不引入 UI 组件库、图表库与状态管理：控制台的表格与卡片用原生实现即可，
体量小、可审计，也避免供应链风险。复杂的多维下钻分析交给 Grafana，
控制台只承担「看状态、定位异常、执行运维动作」。

## 设计约束
- **数据可信**：只展示管理接口返回的真实值或明确标注口径的推导值。接口未提供的数据
  （主机级 GPU/CPU 利用率、告警历史、扩缩容执行记录）显示为空态并说明去哪里查，
  不用随机数或定时器伪造实时指标。
- **表格优先**：可比较的数据（实例、链路、告警、建议）用表格；卡片只用于 KPI 与单条告警。
- **状态不只依赖颜色**：状态同时用图标与文本表达，对比度满足 WCAG 2.1 AA。
- **危险操作可恢复**：隔离、摘除、方案切换、发布重置一律经 `ConfirmDialog` 说明影响范围。
- 详细的页面与验收标准见 [`docs/frontend-console-redesign-plan.md`](../docs/frontend-console-redesign-plan.md)。

## 页面
| 页面 | 路由 | 职责 | 依赖接口 |
|---|---|---|---|
| 总览 | `#/overview` | KPI、吞吐趋势、实例分布、策略槽位、最近告警 | `GET /admin/{stats,stats/history,models,presets,alerts}` |
| 调度方案 | `#/presets` | 五种内置方案一键切换；「可视化构建 / 表达式」双模式编辑 filter/score（评分是代价，最小者胜出）；服务端实时编译校验；可选目标策略槽位 | `GET /admin/presets`、`POST /admin/presets/{name}/apply`、`POST /admin/policies/validate`、`PUT /admin/policies/{name}` |
| 后端实例 | `#/backends` | 负载对比表、搜索/状态/引擎筛选、详情抽屉、隔离恢复、动态注册与摘除 | `GET\|POST /admin/backends`、`DELETE /admin/backends/{id}`、`POST .../cordon\|uncordon`、`GET /admin/models` |
| 调度解释器 | `#/explain` | 模拟一次选路，展示候选过滤与打分排序，回答"为什么路由到 X" | `GET /admin/explain`、`GET /admin/{presets,models}` |
| 运行监控 | `#/runtime` | 吞吐/时延趋势、PD 组与 NCCL 链路、金丝雀发布、集群成员 | `GET /admin/{stats,stats/history,pd,cluster,rollouts}`、`POST /admin/rollouts/{model}/reset` |
| KV Cache 与队列 | `#/cache-queue` | 前缀树规模、命中率与队列趋势、后端缓存明细、按模型排队分布 | `GET /admin/{kvcache,queue,backends,models,stats/history}` |
| 告警与扩缩容 | `#/operations` | 活跃告警、容量趋势、集群负载、扩缩容建议（只读） | `GET /admin/{alerts,autoscale,stats,stats/history}` |

旧 hash 路由继续可用，已有书签不失效。

## 开发
```bash
make web-install        # 安装依赖（首次）
make web-dev            # Vite dev server :5173，热更；/admin 与 /v1 代理到本地网关 :8080
# 另开终端： go run ./cmd/gateway -config configs/gateway.yaml

make web                # 生产构建 → internal/webui/dist
make build              # Go 构建，产物已嵌入
```

`internal/webui/dist/` **已提交到仓库**：保证没装 Node 的机器（生产构建机、
精简 CI 镜像）执行 `go build` 也能得到带控制台的完整二进制。改动前端后
务必重新 `make web` 并把 dist 一起提交。

## 文件
| 路径 | 说明 |
|---|---|
| `src/api.ts` | admin API 客户端与响应类型（对应 `internal/server/server.go`） |
| `src/hooks.ts` | `usePoll` 轮询（页面隐藏时暂停、请求不叠加）、全局刷新上下文、`useToast`、`useEscape` |
| `src/format.ts` | 百分比/字节/紧凑数值/时延/相对时间格式化与占用率档位 |
| `src/components.tsx` | 设计系统组件：`Panel`、`MetricCard`、`DataTable`、`Drawer`、`ConfirmDialog`、状态徽标与异步态 |
| `src/components/PolicyEditor.tsx` | Grafana 风格「可视化构建 / 表达式」双模式策略编辑器 |
| `src/policyBuilder.ts` | 构建器结构化规则模型、Expr 生成与安全子集无损解析 |
| `src/charts.tsx` | 内联 SVG 折线图、柱状图与迷你趋势线 |
| `src/icons.tsx` | 内联 SVG 图标集 |
| `src/App.tsx` | 外壳（侧栏 + 上下文栏）与 hash 路由 |
| `src/pages/*.tsx` | 七个页面 |
