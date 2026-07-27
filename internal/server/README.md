# internal/server/ — HTTP 接入层

## 职责
- 标准库 `net/http`（Go 1.22 方法路由），不引入 web 框架；
- 单端口承载数据面（`/v1/*` → proxy）、自身指标（`GET /metrics`）、
  存活探针（`GET /healthz`）与管理面（`/admin/*`，端点清单见 `api/README.md`）；
- 流式友好：不设全局 WriteTimeout（长流合法），仅设 ReadHeaderTimeout 防慢速头攻击；
- 优雅退出：ctx 取消后 `http.Server.Shutdown`（等待在途请求完成，上限 30s）；
- 管理变更类端点（cordon / 策略热更）记录操作审计日志（slog INFO）。

## 接口
```go
func New(cfg, reg, pol, sched, tree, pdMgr, gw, px) *Server
func (s *Server) SetAlerting(eng *alerting.Engine, sc *alerting.Autoscaler) // 可传 nil
func (s *Server) Handler() http.Handler   // 测试/嵌入用
func (s *Server) Run(ctx context.Context) error
```

## 文件
`server.go`（路由注册 + 全部管理端点 + 生命周期）、`server_test.go`。
