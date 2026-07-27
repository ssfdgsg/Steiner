# internal/tracing/ — 分布式追踪（OpenTelemetry）

## 职责
初始化 OTel 全局 TracerProvider：OTLP/HTTP 导出器、ParentBased 比例采样、
资源属性（service.name / service.version），并注册 W3C TraceContext + Baggage 传播器。

## 设计取舍
- **打点统一走 otel 全局**：未启用追踪时全局为 noop 实现，代理层打点开销可忽略，
  业务代码不感知开关；
- **传播器无条件注册**：即便本实例不导出 span，也把入站 `traceparent` 透传给上游，
  保持整条链路不断；
- 关停走 `TracerProvider.Shutdown`（main 优雅退出时冲刷缓冲的 span）。

## 每请求 span 链（打点在 internal/proxy）
```
POST /v1/chat/completions            根 span（server）：model/route/stream/request_id/状态码
├── gateway.pick (attempt=0)         选路：strategy、选中 backend 或失败原因
│   └── gateway.queue_wait           仅当排队：等容量的完整时长
├── gateway.forward (client)         转发：backend、状态码
│   ├── event first_byte             TTFT（gateway.ttft_seconds）
│   ├── event stream_interrupt       流中断（如发生）
│   └── attr prompt/completion_tokens 流尾 usage
├── gateway.pick (attempt=1)         重试路径：每次重试各有 pick+forward
└── gateway.forward ...
PD 分离：gateway.pd.prefill / gateway.pd.decode 替代 forward（含 bootstrap room）。
```

## 关联关系
- 响应头返回 `X-Trace-Id`（根 span trace ID）与 `X-Request-Id`，
  根 span 带 `gateway.request_id` 属性——日志、指标、追踪三方互查；
- 上游请求注入 `traceparent`：vLLM/SGLang 若启用 OTel 可续接同一条 trace，
  实现「客户端 → 网关 → 推理引擎」全链路。

## 配置（configs/gateway.example.yaml 的 tracing 段）
`enabled / endpoint（OTLP HTTP host:port）/ insecure / sample_ratio / service_name / headers`。

## 文件
`tracing.go`（Setup 与全局装配）；打点助手在 `internal/proxy/trace.go`，
链路结构测试在 `internal/proxy/tracing_test.go`（内存导出器断言 span 链/TTFT 事件/传播头）。
