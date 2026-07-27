// 上游响应 token 用量提取。
//
// 非流式：usage 对象位于 JSON 响应体内（通常在末尾）；
// 流式：vLLM / SGLang 在最后一个 SSE 块携带 usage（OpenAI 协议需
// stream_options.include_usage，两家引擎默认或可配置输出）。
// 两种形态统一用"尾部缓冲"方案：透传过程中保留响应最后 tailCap 字节，
// 流结束后从尾部反查最后一个 "usage" 对象解析，避免全量缓冲大响应。
package proxy

import (
	"bytes"
	"encoding/json"
)

// tailCap 尾部缓冲容量。usage 总在响应末尾，32KiB 足够覆盖末块。
const tailCap = 32 * 1024

// tailBuffer 保留写入内容的最后 tailCap 字节。
type tailBuffer struct {
	buf []byte
}

// Write 追加数据，超容量时丢弃最旧部分。
func (t *tailBuffer) Write(p []byte) {
	if len(p) >= tailCap {
		t.buf = append(t.buf[:0], p[len(p)-tailCap:]...)
		return
	}
	if overflow := len(t.buf) + len(p) - tailCap; overflow > 0 {
		t.buf = append(t.buf[:0], t.buf[overflow:]...)
	}
	t.buf = append(t.buf, p...)
}

// usagePayload usage 对象的关注字段。
type usagePayload struct {
	PromptTokens     float64 `json:"prompt_tokens"`
	CompletionTokens float64 `json:"completion_tokens"`
}

// parseUsageTail 从响应尾部提取最后一个非空 usage。
// 返回 ok=false 表示未携带 usage 或数值为零（如流式末块 usage:null）。
func parseUsageTail(tail []byte) (prompt, completion float64, ok bool) {
	key := []byte(`"usage"`)
	// 从后往前找，跳过流式中间块的 "usage":null。
	for idx := bytes.LastIndex(tail, key); idx >= 0; idx = bytes.LastIndex(tail[:idx], key) {
		rest := tail[idx+len(key):]
		colon := bytes.IndexByte(rest, ':')
		if colon < 0 {
			continue
		}
		var u usagePayload
		dec := json.NewDecoder(bytes.NewReader(rest[colon+1:]))
		if err := dec.Decode(&u); err != nil {
			continue
		}
		if u.PromptTokens > 0 || u.CompletionTokens > 0 {
			return u.PromptTokens, u.CompletionTokens, true
		}
	}
	return 0, 0, false
}
