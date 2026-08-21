// 上游响应 token 用量提取。
//
// 非流式：usage 对象位于 JSON 响应体内（通常在末尾）；
// 流式：vLLM / SGLang 在最后一个 SSE 块携带 usage（OpenAI 协议需
// stream_options.include_usage，两家引擎默认或可配置输出）。
// 两种形态统一用"尾部缓冲"方案：透传过程中保留响应最后 tailCap 字节，
// 流结束后从尾部提取 usage，避免全量缓冲大响应。
//
// 提取走严格路径，只承认两类真实 usage：
//  1. 流式：按 `data: ` 行切分，最后一个 JSON 帧（忽略 [DONE]）带非 null 的 usage；
//  2. 非流式：尾部整体是一份合法 JSON 且顶层带 usage；或尾部是超 tailCap 响应
//     被截断后的 JSON 文档后缀——从后往前找处于对象键位（`{`/`,` 之后）的
//     `"usage"`，其值可解析为 JSON 且其后只剩 JSON 收尾字符。
//
// 其余情形（如模型输出内容文本里恰好出现 `"usage":{...}`）一律不承认，避免误记账。
// 识别到 usage 帧即 ok=true（即使 prompt/completion 全为零），记账与否由调用方决定。
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

// parseUsageTail 从响应尾部提取 token 用量（严格路径）。
// 返回 ok=false 表示尾部不携带可承认的 usage。
func parseUsageTail(tail []byte) (prompt, completion float64, ok bool) {
	if len(bytes.TrimSpace(tail)) == 0 {
		return 0, 0, false
	}
	if hasSSELines(tail) {
		return parseSSEUsage(tail)
	}
	return parseJSONUsage(tail)
}

// hasSSELines 判断尾部是否为 SSE 流：存在以 `data:` 开头的帧行。
// 合法 JSON 文档的字符串不能跨行，因此 JSON 行不可能以 `data:` 开头，判定可靠。
func hasSSELines(tail []byte) bool {
	for _, line := range bytes.Split(tail, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			return true
		}
	}
	return false
}

// parseSSEUsage 流式：按 `data: ` 行解析，取最后一个 JSON 帧（忽略 `data: [DONE]`），
// 该帧顶层对象必须带非 null 的 usage 才承认；中间块的 usage:null 天然被忽略。
func parseSSEUsage(tail []byte) (prompt, completion float64, ok bool) {
	var frames [][]byte
	for _, line := range bytes.Split(tail, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		frames = append(frames, payload)
	}
	// 从后往前找最后一个可解析的 JSON 帧；该帧不含 usage（含 usage:null）即整段不承认。
	for i := len(frames) - 1; i >= 0; i-- {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(frames[i], &doc); err != nil {
			continue // 被截断/非 JSON 的行不算"JSON 帧"
		}
		raw, has := doc["usage"]
		if !has {
			return 0, 0, false
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return 0, 0, false // usage:null = 未携带用量（协议未开 include_usage）
		}
		var u usagePayload
		if err := json.Unmarshal(raw, &u); err != nil {
			return 0, 0, false
		}
		return u.PromptTokens, u.CompletionTokens, true
	}
	return 0, 0, false
}

// parseJSONUsage 非流式：优先整份解析（尾部整体是合法 JSON 且顶层带 usage）；
// 否则按"JSON 文档后缀"处理——尾部可能是超 tailCap 响应被截断后的片段，
// 从后往前找处于对象键位（`{`/`,` 之后）的 `"usage"`，其值可解析且其后只剩
// JSON 收尾字符即承认。
func parseJSONUsage(tail []byte) (prompt, completion float64, ok bool) {
	if doc, err := parseJSONObject(tail); err == nil {
		if raw, has := doc["usage"]; has {
			return decodeUsage(raw)
		}
	}
	key := []byte(`"usage"`)
	trimmed := bytes.TrimRight(tail, " \t\r\n")
	for idx := bytes.LastIndex(trimmed, key); idx >= 0; idx = bytes.LastIndex(trimmed[:idx], key) {
		if !usageKeyPosition(trimmed, idx) {
			continue
		}
		value := bytes.TrimSpace(trimmed[idx+len(key):])
		if !bytes.HasPrefix(value, []byte(":")) {
			continue
		}
		value = bytes.TrimSpace(value[1:])
		var u usagePayload
		dec := json.NewDecoder(bytes.NewReader(value))
		if err := dec.Decode(&u); err != nil {
			continue
		}
		// 值之后只允许 JSON 收尾（空白与 }/]），排除"伪 usage 后还跟着正文"。
		if !jsonClosersOnly(value[dec.InputOffset():]) {
			continue
		}
		return u.PromptTokens, u.CompletionTokens, true
	}
	return 0, 0, false
}

// parseJSONObject 将整段作为 JSON 对象解析，失败返回错误。
func parseJSONObject(tail []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(tail, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// decodeUsage 从 usage 原始值提取 token 数。usage 为 null 或对象都算"携带 usage"，
// 数值全为零也 ok=true（记账与否由调用方决定）。
func decodeUsage(raw json.RawMessage) (prompt, completion float64, ok bool) {
	var u usagePayload
	if err := json.Unmarshal(raw, &u); err != nil {
		return 0, 0, false
	}
	return u.PromptTokens, u.CompletionTokens, true
}

// usageKeyPosition 判断 `"usage"` 是否处于对象键位：其前一个非空白字符必须是
// `{` 或 `,`（内容文本中的 `"usage"` 通常不在该位置，如行首缩进/中文标点之后）。
func usageKeyPosition(tail []byte, idx int) bool {
	for i := idx - 1; i >= 0; i-- {
		switch tail[i] {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return tail[i] == '{' || tail[i] == ','
	}
	return false
}

// jsonClosersOnly 判断剩余内容是否只有 JSON 收尾字符（空白与 }/]）。
func jsonClosersOnly(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n', '}', ']':
		default:
			return false
		}
	}
	return true
}
