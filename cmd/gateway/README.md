# cmd/gateway/ — 进程入口

## 职责
唯一的可执行入口。只做**装配与生命周期管理**，不含业务逻辑：

1. 解析命令行参数（`-config` 配置路径，默认 `configs/gateway.yaml`；`-version`）；
2. `internal/config` 加载并校验配置（策略/告警表达式编译失败即启动失败，左移暴露）；
3. 按依赖顺序装配：
   `backend 注册表 → policy 引擎 → kvcache → scheduler → pd → metrics(自身指标+collector)
   → proxy → session/queue（可选，经 Set* 注入）→ alerting/autoscale（可选）→ server`；
4. 启动后台循环：健康检查（翻转回调联动粘性解绑与排队唤醒）、指标直采、
   PromQL 轮询、前缀树清理、运行态 gauge 刷新；
5. 监听 SIGINT/SIGTERM，`server.Shutdown` 优雅退出（等待在途请求，上限 30s），
   后台循环随 ctx 统一取消。

## 文件
`main.go`。

## 边界
- 不做任何请求处理、指标解析或调度决策；
- main 是唯一知道全部具体类型的装配点。
