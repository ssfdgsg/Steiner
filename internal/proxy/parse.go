// 请求解析：从 OpenAI 兼容请求体中提取调度所需信息
// （模型名、流式标记、提示词文本、会话 ID、优先级、多模态特征与 token 估算）。
// 解析是尽力而为的：非 JSON 或字段缺失不报错，只影响可用的调度信号。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"

	"ai-gateway/internal/scheduler"
)

// 多模态分段的 token 成本经验权重：图像按主流 ViT patch 数量级折算，
// 音频/视频为保守估计。用于容量准入与调度表达式的成本估算，非精确计费。
const (
	imageTokenWeight = 576
	audioTokenWeight = 750
	videoTokenWeight = 1500
)

// mmFeatures 多模态分段计数。
type mmFeatures struct {
	images, audios, videos int
}

// requestMeta 是不保留大字段的轻量请求信封。专用扫描器跳过
// messages/prompt/input，因此大 query 快路径不会为 prompt 再分配大字符串或 map 树。
type requestMeta struct {
	Model    string  `json:"model"`
	Stream   bool    `json:"stream"`
	Priority float64 `json:"priority"`
	User     string  `json:"user"`
}

// parseRequestMeta 只提取选路前必需的小字段。用于无需 KV 前缀、prompt 成本特征、
// PD 文档或模型改写的常规转发路径。
func parseRequestMeta(r *http.Request, body []byte) *scheduler.Request {
	var meta requestMeta
	if len(body) > 0 {
		if !scanRequestMeta(body, &meta) {
			// 非标准/损坏 JSON 保持原有尽力解析语义，交给 encoding/json 兜底。
			_ = json.Unmarshal(body, &meta)
		}
	}
	req := &scheduler.Request{
		Model: meta.Model, Stream: meta.Stream, Priority: meta.Priority, SessionID: meta.User,
	}
	applySessionHeader(r, req)
	return req
}

// scanRequestMeta 扫描 JSON 顶层对象，只解码四个小字段；嵌套的大 messages、
// prompt、input 值按字节跳过。它不创建大字符串，也不构造反射 map。
func scanRequestMeta(body []byte, meta *requestMeta) bool {
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return false
	}
	i++
	for {
		i = skipJSONSpace(body, i)
		if i >= len(body) {
			return false
		}
		if body[i] == '}' {
			return skipJSONSpace(body, i+1) == len(body)
		}
		keyStart := i
		keyEnd, ok := skipJSONString(body, i)
		if !ok {
			return false
		}
		i = skipJSONSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return false
		}
		i = skipJSONSpace(body, i+1)
		valueStart := i
		valueEnd, ok := skipJSONValue(body, i)
		if !ok {
			return false
		}
		key := body[keyStart+1 : keyEnd-1]
		field := ""
		switch {
		case bytes.Equal(key, []byte("model")):
			field = "model"
		case bytes.Equal(key, []byte("stream")):
			field = "stream"
		case bytes.Equal(key, []byte("priority")):
			field = "priority"
		case bytes.Equal(key, []byte("user")):
			field = "user"
		case bytes.IndexByte(key, '\\') >= 0:
			// JSON 允许对象键使用转义（如 "mo\u0064el"）；只为罕见转义键分配。
			if err := json.Unmarshal(body[keyStart:keyEnd], &field); err != nil {
				return false
			}
		}
		switch field {
		case "model":
			_ = json.Unmarshal(body[valueStart:valueEnd], &meta.Model)
		case "stream":
			_ = json.Unmarshal(body[valueStart:valueEnd], &meta.Stream)
		case "priority":
			_ = json.Unmarshal(body[valueStart:valueEnd], &meta.Priority)
		case "user":
			_ = json.Unmarshal(body[valueStart:valueEnd], &meta.User)
		}
		i = skipJSONSpace(body, valueEnd)
		if i >= len(body) {
			return false
		}
		switch body[i] {
		case ',':
			i++
		case '}':
			return skipJSONSpace(body, i+1) == len(body)
		default:
			return false
		}
	}
}

func skipJSONSpace(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONString 返回结束引号后一位。bytes.IndexByte 使大纯文本字段走快速扫描。
func skipJSONString(body []byte, i int) (int, bool) {
	if i >= len(body) || body[i] != '"' {
		return i, false
	}
	for i++; i < len(body); {
		rel := bytes.IndexByte(body[i:], '"')
		if rel < 0 {
			return len(body), false
		}
		quote := i + rel
		backslashes := 0
		for j := quote - 1; j >= i && body[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return quote + 1, true
		}
		i = quote + 1
	}
	return len(body), false
}

func skipJSONValue(body []byte, i int) (int, bool) {
	if i >= len(body) {
		return i, false
	}
	switch body[i] {
	case '"':
		return skipJSONString(body, i)
	case '{', '[':
		var closers [64]byte
		depth := 0
		for i < len(body) {
			switch body[i] {
			case '"':
				var ok bool
				i, ok = skipJSONString(body, i)
				if !ok {
					return i, false
				}
				continue
			case '{':
				if depth == len(closers) {
					return i, false
				}
				closers[depth] = '}'
				depth++
			case '[':
				if depth == len(closers) {
					return i, false
				}
				closers[depth] = ']'
				depth++
			case '}', ']':
				if depth == 0 || body[i] != closers[depth-1] {
					return i, false
				}
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return i, false
	default:
		start := i
		for i < len(body) && body[i] != ',' && body[i] != '}' && body[i] != ']' &&
			body[i] != ' ' && body[i] != '\t' && body[i] != '\r' && body[i] != '\n' {
			i++
		}
		return i, i > start
	}
}

// parseRequest 解析请求体，返回调度信息与已解析的 JSON 文档（PD 转发需要改写文档）。
// body 非 JSON 对象时 doc 返回 nil。
func parseRequest(r *http.Request, body []byte) (*scheduler.Request, map[string]interface{}) {
	req := &scheduler.Request{}
	doc := parseRequestDocument(body)
	if doc != nil {
		populateRequestBase(req, doc)
		populatePromptFeatures(req, doc, -1)
	}
	applySessionHeader(r, req)
	return req, doc
}

// parseRequestDocument 解析请求体为 JSON 文档。
// 数值用 json.Number 保留原文（dec.UseNumber）：M16 修复——文档经模型名改写 /
// PD 转发整体重序列化时，>2^53 的大整数（max_tokens、毫秒时间戳、seed 等）
// 不再被 float64 舍入，重序列化原样输出。
func parseRequestDocument(body []byte) map[string]interface{} {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]interface{}
	if dec.Decode(&doc) != nil {
		return nil
	}
	// 与 json.Unmarshal 语义一致：拒绝尾随非空白内容（如 {"a":1}{"b":2}）。
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil
	}
	return doc
}

// toFloat64 读取 JSON 数字字段。UseNumber 解码路径产生 json.Number（保留原文），
// 原生/外部解码路径产生 float64；两种形态归一为 float64 供调度数值使用。
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func populateRequestBase(req *scheduler.Request, doc map[string]interface{}) {
	if v, ok := doc["model"].(string); ok {
		req.Model = v
	}
	if v, ok := doc["stream"].(bool); ok {
		req.Stream = v
	}
	if v, ok := toFloat64(doc["priority"]); ok {
		req.Priority = v
	}
	if v, ok := doc["user"].(string); ok {
		req.SessionID = v
	}
}

func applySessionHeader(r *http.Request, req *scheduler.Request) {
	// 请求头优先级更高：显式会话头覆盖 body 中的 user 字段。
	if v := r.Header.Get("X-Session-Id"); v != "" {
		req.SessionID = v
	}
}

// promptAccumulator 在遍历解码文档时始终统计完整逻辑长度，但只保留 limit 字节。
// limit=0 表示不保留文本（零额外 prompt 拷贝），limit<0 表示完整保留。
type promptAccumulator struct {
	sb     strings.Builder
	limit  int
	length int
	mm     mmFeatures
}

func (a *promptAccumulator) writeString(s string) {
	a.length += len(s)
	if a.limit == 0 {
		return
	}
	if a.limit < 0 {
		a.sb.WriteString(s)
		return
	}
	remaining := a.limit - a.sb.Len()
	if remaining <= 0 {
		return
	}
	if len(s) > remaining {
		s = s[:remaining]
	}
	a.sb.WriteString(s)
}

func (a *promptAccumulator) writeByte(c byte) {
	a.length++
	if a.limit < 0 || a.sb.Len() < a.limit {
		a.sb.WriteByte(c)
	}
}

// populatePromptFeatures 提取完整长度、多模态计数与 token 估算；promptLimit 决定
// 是否以及保留多少前缀文本，避免为不使用缓存的路由复制整个大 query。
func populatePromptFeatures(req *scheduler.Request, doc map[string]interface{}, promptLimit int) {
	a := promptAccumulator{limit: promptLimit}
	accumulatePrompt(doc, &a)
	req.PromptText = a.sb.String()
	req.PromptLen = a.length
	req.ImageCount = a.mm.images
	req.AudioCount = a.mm.audios
	req.VideoCount = a.mm.videos
	req.IsMultimodal = a.mm.images+a.mm.audios+a.mm.videos > 0
	// 文本按 ~4 字节/token 粗估（中英混合的工程近似），多模态按权重折算。
	req.PromptTokensEst = req.PromptLen/4 +
		a.mm.images*imageTokenWeight + a.mm.audios*audioTokenWeight + a.mm.videos*videoTokenWeight
}

// extractPrompt 提取用于前缀匹配的文本并统计多模态分段：
//   - chat/completions: 按顺序拼接 messages 的 role 与内容
//     （多模态分段以内容哈希占位参与匹配，见 contentText）；
//   - completions: prompt 字符串或字符串数组；
//   - embeddings: input 字符串或字符串数组。
func extractPrompt(doc map[string]interface{}, mm *mmFeatures) string {
	a := promptAccumulator{limit: -1}
	accumulatePrompt(doc, &a)
	*mm = a.mm
	return a.sb.String()
}

func accumulatePrompt(doc map[string]interface{}, a *promptAccumulator) {
	if msgs, ok := doc["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if role, ok := msg["role"].(string); ok {
				a.writeString(role)
				a.writeByte(':')
			}
			accumulateContent(msg["content"], a)
			a.writeByte('\n')
		}
		return
	}
	if v, ok := doc["prompt"]; ok {
		accumulateContent(v, a)
		return
	}
	if v, ok := doc["input"]; ok {
		accumulateContent(v, a)
	}
}

// contentText 把 string / []string / 多模态分段数组归一化为纯文本。
// 图像/音频/视频分段计数并以内容哈希占位符参与文本——相同素材的多轮请求
// 仍能在前缀树上命中同一前缀，享受 KV 亲和收益。
func contentText(v interface{}, mm *mmFeatures) string {
	a := promptAccumulator{limit: -1}
	accumulateContent(v, &a)
	*mm = a.mm
	return a.sb.String()
}

func accumulateContent(v interface{}, a *promptAccumulator) {
	switch c := v.(type) {
	case string:
		a.writeString(c)
	case []interface{}:
		for _, item := range c {
			switch part := item.(type) {
			case string:
				a.writeString(part)
			case map[string]interface{}:
				if t, ok := part["text"].(string); ok {
					a.writeString(t)
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "image_url":
					a.mm.images++
					a.writeString(mmPlaceholder("img", part["image_url"]))
				case "input_audio":
					a.mm.audios++
					a.writeString(mmPlaceholder("aud", part["input_audio"]))
				case "video_url":
					a.mm.videos++
					a.writeString(mmPlaceholder("vid", part["video_url"]))
				}
			}
		}
	}
}

// mmPlaceholder 多模态分段的稳定占位符：对分段内容（URL 或 base64 数据）
// 取 FNV-64 哈希，同素材同占位。
func mmPlaceholder(kind string, v interface{}) string {
	raw, _ := json.Marshal(v)
	h := fnv.New64a()
	_, _ = h.Write(raw)
	return "[" + kind + ":" + strconv.FormatUint(h.Sum64(), 16) + "]"
}
