# internal/server/ — HTTP 接入层

## 职责
- 标准库 `net/http`（Go 1.22 方法路由），不引入 web 框架；
- 单端口承载数据面（`/v1/*` → proxy）、自身指标（`GET /metrics`）、
  存活探针（`GET /healthz`）与管理面（`/admin/*`，端点清单见 `api/README.md`）；
- 流式友好：不设全局 WriteTimeout（长流合法），设 ReadHeaderTimeout 防慢速头攻击、
  ReadTimeout 防慢速请求体上传；
- 优雅退出：ctx 取消后 `http.Server.Shutdown`（等待在途请求完成，上限 30s）；
- 管理变更类端点（cordon / 策略热更）记录操作审计日志（slog INFO）。

## 安全（C1/H1 修复后）
- `/admin/*` 全部要求 `Authorization: Bearer <token>`（`server.admin_token`，恒定时间比较）；
  令牌未配置时网关拒绝启动。`/metrics`、`/healthz`、`/v1/*` 不要求认证。
- 管理面变更类请求（POST/PUT/PATCH/DELETE）强制 `Content-Type: application/json`，
  非 JSON 一律 415（切断跨站表单投递的 CSRF 前置）。
- 默认监听 `127.0.0.1:8080`（仅本机回环）；对外暴露需显式配置。

## 控制台数据面约定
控制台（`/admin/ui/`）只允许消费管理端点的真实返回值，因此这里为它提供三个只读视图：

- `GET /admin/stats`：`metrics.Aggregate`（与 `/metrics` 同源）+ 后端池汇总 + 排队即时深度；
- `GET /admin/stats/history`：`metrics.History` 的采样缓冲（进程内、重启清空）；
- `GET /admin/models`：模型路由拓扑与池内健康计数——后端快照不含模型标签，
  一个实例可服务多个路由，故「模型 → 实例」映射必须由注册表给出。

能力未启用时统一回 `{"enabled": false}` 而非零值，让控制台能区分「未配置」与「值为 0」。

## 接口
```go
func New(cfg, reg, pol, sched, tree, pdMgr, gw, px) *Server
func (s *Server) SetAlerting(eng *alerting.Engine, sc *alerting.Autoscaler) // 可传 nil
func (s *Server) SetCluster(c *cluster.Manager)   // 可传 nil
func (s *Server) SetStore(st *store.Store)        // 可传 nil
func (s *Server) SetRollouts(ro *rollout.Manager) // 可传 nil
func (s *Server) SetStats(h *metrics.History)     // 可传 nil
func (s *Server) Handler() http.Handler   // 测试/嵌入用
func (s *Server) Run(ctx context.Context) error
```

## 文件
`server.go`（路由注册 + 全部管理端点 + 生命周期）、`server_test.go`、
`presets_test.go`（方案切换与控制台静态资源）、`stats_test.go`（控制台数据源端点）。
