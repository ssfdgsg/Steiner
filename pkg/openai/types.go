// Package openai 定义网关对外的 OpenAI 兼容协议类型。
// 放在 pkg/（而非 internal/）是因为压测脚本、mock 后端等外部工具同样需要。
//
// 设计原则：网关不是协议校验器，请求体对后端原样透传；调度所需字段的
// 提取采用宽松的 map 解析（见 internal/proxy/parse.go），本包只保留
// 网关自身产生的响应结构。
package openai

// ModelList /v1/models 响应。
type ModelList struct {
	Object string      `json:"object"`
	Data   []ModelItem `json:"data"`
}

// ModelItem /v1/models 中的单个模型。
type ModelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ErrorResponse OpenAI 风格错误响应（网关自身产生的错误统一走此结构）。
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody 错误体。
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    int    `json:"code"`
}
