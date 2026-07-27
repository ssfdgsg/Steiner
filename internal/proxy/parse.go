// 请求解析：从 OpenAI 兼容请求体中提取调度所需信息
// （模型名、流式标记、提示词文本、会话 ID、优先级、多模态特征与 token 估算）。
// 解析是尽力而为的：非 JSON 或字段缺失不报错，只影响可用的调度信号。
package proxy

import (
	"encoding/json"
	"hash/fnv"
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

// parseRequest 解析请求体，返回调度信息与已解析的 JSON 文档（PD 转发需要改写文档）。
// body 非 JSON 对象时 doc 返回 nil。
func parseRequest(r *http.Request, body []byte) (*scheduler.Request, map[string]interface{}) {
	req := &scheduler.Request{}
	var doc map[string]interface{}
	if len(body) > 0 && json.Unmarshal(body, &doc) != nil {
		doc = nil
	}
	if doc != nil {
		if v, ok := doc["model"].(string); ok {
			req.Model = v
		}
		if v, ok := doc["stream"].(bool); ok {
			req.Stream = v
		}
		if v, ok := doc["priority"].(float64); ok {
			req.Priority = v
		}
		if v, ok := doc["user"].(string); ok {
			req.SessionID = v
		}
		var mm mmFeatures
		req.PromptText = extractPrompt(doc, &mm)
		req.PromptLen = len(req.PromptText)
		req.ImageCount = mm.images
		req.AudioCount = mm.audios
		req.VideoCount = mm.videos
		req.IsMultimodal = mm.images+mm.audios+mm.videos > 0
		// 文本按 ~4 字节/token 粗估（中英混合的工程近似），多模态按权重折算。
		req.PromptTokensEst = req.PromptLen/4 +
			mm.images*imageTokenWeight + mm.audios*audioTokenWeight + mm.videos*videoTokenWeight
	}
	// 请求头优先级更高：显式会话头覆盖 body 中的 user 字段。
	if v := r.Header.Get("X-Session-Id"); v != "" {
		req.SessionID = v
	}
	return req, doc
}

// extractPrompt 提取用于前缀匹配的文本并统计多模态分段：
//   - chat/completions: 按顺序拼接 messages 的 role 与内容
//     （多模态分段以内容哈希占位参与匹配，见 contentText）；
//   - completions: prompt 字符串或字符串数组；
//   - embeddings: input 字符串或字符串数组。
func extractPrompt(doc map[string]interface{}, mm *mmFeatures) string {
	if msgs, ok := doc["messages"].([]interface{}); ok {
		var sb strings.Builder
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if role, ok := msg["role"].(string); ok {
				sb.WriteString(role)
				sb.WriteByte(':')
			}
			sb.WriteString(contentText(msg["content"], mm))
			sb.WriteByte('\n')
		}
		return sb.String()
	}
	if v, ok := doc["prompt"]; ok {
		return contentText(v, mm)
	}
	if v, ok := doc["input"]; ok {
		return contentText(v, mm)
	}
	return ""
}

// contentText 把 string / []string / 多模态分段数组归一化为纯文本。
// 图像/音频/视频分段计数并以内容哈希占位符参与文本——相同素材的多轮请求
// 仍能在前缀树上命中同一前缀，享受 KV 亲和收益。
func contentText(v interface{}, mm *mmFeatures) string {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		var sb strings.Builder
		for _, item := range c {
			switch part := item.(type) {
			case string:
				sb.WriteString(part)
			case map[string]interface{}:
				if t, ok := part["text"].(string); ok {
					sb.WriteString(t)
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "image_url":
					mm.images++
					sb.WriteString(mmPlaceholder("img", part["image_url"]))
				case "input_audio":
					mm.audios++
					sb.WriteString(mmPlaceholder("aud", part["input_audio"]))
				case "video_url":
					mm.videos++
					sb.WriteString(mmPlaceholder("vid", part["video_url"]))
				}
			}
		}
		return sb.String()
	}
	return ""
}

// mmPlaceholder 多模态分段的稳定占位符：对分段内容（URL 或 base64 数据）
// 取 FNV-64 哈希，同素材同占位。
func mmPlaceholder(kind string, v interface{}) string {
	raw, _ := json.Marshal(v)
	h := fnv.New64a()
	_, _ = h.Write(raw)
	return "[" + kind + ":" + strconv.FormatUint(h.Sum64(), 16) + "]"
}
