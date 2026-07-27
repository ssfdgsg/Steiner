# web/ — React 管理控制台

网关的可视化管理界面。构建产物输出到 `internal/webui/dist/`，由 `go:embed`
打进网关二进制，运维**无需部署额外的静态服务器**，启动网关后访问
`http://<网关地址>/admin/ui/` 即可。

## 技术选型
| 能力 | 选型 | 理由 |
|---|---|---|
| 框架 | React 18 + TypeScript | 需求明确指定 React；TS 让 admin API 响应结构有编译期约束 |
| 构建 | Vite 5 | 零配置起步、构建快、产物小（gzip 约 54 KB） |
| 路由 | hash 路由（`#/presets`） | 嵌入式静态托管无需服务端改写规则，刷新不 404 |
| 状态/请求 | 原生 `fetch` + 自研 `usePoll` | 只有轮询只读接口与少量写操作，引入状态库得不偿失 |
| 样式 | 原生 CSS 变量 | 深浅色自适应；无 CDN 依赖，兼容内网隔离环境 |

刻意不引入 UI 组件库、图表库与状态管理：控制台的表格与卡片用原生实现即可，
体量小、可审计，也避免供应链风险。

## 页面
| 页面 | 职责 | 依赖接口 |
|---|---|---|
| 调度方案 | 五种内置方案卡片一键切换（含中文说明与表达式预览）；手写 filter/score 内联编辑；可选目标策略槽位 | `GET /admin/presets`、`POST /admin/presets/{name}/apply`、`PUT /admin/policies/{name}` |
| 后端实例 | 实时负载表格（在途/运行/排队/KV 占用/命中率/吞吐）、隔离与恢复、动态注册与摘除 | `GET|POST /admin/backends`、`DELETE /admin/backends/{id}`、`POST .../cordon\|uncordon` |
| 调度解释器 | 模拟一次选路，展示逐后端过滤结果与打分排序，回答"为什么路由到 X" | `GET /admin/explain` |
| 运行态 | KV 前缀树、容量排队、PD 组与 NCCL 链路、告警、扩缩容建议、集群成员 | `GET /admin/{kvcache,queue,pd,alerts,autoscale,cluster}` |

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
| `src/hooks.ts` | `usePoll` 轮询、`useToast` 提示 |
| `src/format.ts` | 百分比/字节/数值格式化与占用率配色档位 |
| `src/components.tsx` | 共享展示组件（状态徽标、占用率条、提示浮层） |
| `src/App.tsx` | 外壳与 hash 路由 |
| `src/pages/*.tsx` | 四个页面 |
