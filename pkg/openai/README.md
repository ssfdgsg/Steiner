# pkg/openai/ — OpenAI 协议类型

## 职责
定义网关对外的 OpenAI 兼容协议类型（模型清单、错误响应）。
放在 `pkg/`（而非 internal/）是因为压测脚本、mock 后端等外部工具同样可复用。

## 设计原则：网关不是协议校验器
请求体对后端**原样透传**；调度所需字段（model / stream / 提示词 / 会话键 /
priority）的提取采用宽松的 map 解析，实现在 `internal/proxy/parse.go`——
未知字段不校验、不丢弃，绝不因客户端使用新参数而 400。
因此本包刻意只保留**网关自身产生**的响应结构：

```go
type ModelList  struct { Object string; Data []ModelItem }   // GET /v1/models
type ModelItem  struct { ID, Object string; Created int64; OwnedBy string }
type ErrorResponse struct { Error ErrorBody }                 // 统一错误结构
type ErrorBody  struct { Message, Type string; Code int }
```

## 文件
`types.go`。
