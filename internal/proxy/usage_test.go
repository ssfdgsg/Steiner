package proxy

import (
	"bytes"
	"strings"
	"testing"
)

// TestParseUsageTail_非流式JSON 验证 OpenAI 风格 JSON 响应体的 usage 提取。
func TestParseUsageTail_非流式JSON(t *testing.T) {
	body := []byte(`{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"你好"}}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`)
	prompt, completion, ok := parseUsageTail(body)
	if !ok || prompt != 12 || completion != 34 {
		t.Fatalf("提取结果不符: prompt=%v completion=%v ok=%v", prompt, completion, ok)
	}
}

// TestParseUsageTail_流式末块 验证 SSE 末块 usage 提取，且跳过中间块的 usage:null。
func TestParseUsageTail_流式末块(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"a"}}],"usage":null}`,
		`data: {"choices":[{"delta":{"content":"b"}}],"usage":null}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	prompt, completion, ok := parseUsageTail([]byte(sse))
	if !ok || prompt != 5 || completion != 7 {
		t.Fatalf("提取结果不符: prompt=%v completion=%v ok=%v", prompt, completion, ok)
	}
}

// TestParseUsageTail_无usage 验证无 usage 或全为 null 时返回 ok=false。
func TestParseUsageTail_无usage(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"choices":[{"message":{"content":"x"}}]}`),
		[]byte(`data: {"usage":null}` + "\n\ndata: [DONE]\n"),
		[]byte(``),
	}
	for i, c := range cases {
		if _, _, ok := parseUsageTail(c); ok {
			t.Fatalf("用例 %d 不应提取到 usage", i)
		}
	}
}

// TestParseUsageTail_内容文本伪usage 验证 L1 第一面的修复：
// 纯文本/裸流模式下，模型输出内容里恰好包含形如 "usage":{...} 的 JSON 文本
// （如引用工具返回、演示 JSON）时，严格路径不再把它当作真实用量——
// 修复后应 ok=false（伪 usage 不再误计）。
func TestParseUsageTail_内容文本伪usage(t *testing.T) {
	body := []byte(`好的,这是你请求的 JSON 示例:
"usage": {"prompt_tokens": 999, "completion_tokens": 888, "total_tokens": 1887}`)
	prompt, completion, ok := parseUsageTail(body)
	if ok {
		t.Fatalf("L1 修复失败：内容文本中的伪 usage 不应被计入（严格路径）: prompt=%v completion=%v",
			prompt, completion)
	}
}

// TestParseUsageTail_全零usage漏计 验证 L1 第二面的修复：
// 合法但全为零的 usage（缓存全命中、max_tokens=0）此前被当作"无 usage"漏计。
// 修复后（识别到 usage 帧即 ok=true，记账与否由调用方决定）应 ok=true。
func TestParseUsageTail_全零usage漏计(t *testing.T) {
	body := []byte(`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` + "\n\ndata: [DONE]\n")
	prompt, completion, ok := parseUsageTail(body)
	if !ok {
		t.Fatalf("L1 修复失败：全零 usage 帧应被识别（ok=true）: prompt=%v completion=%v",
			prompt, completion)
	}
}

func TestTailBuffer_滚动保留(t *testing.T) {
	var tb tailBuffer
	// 写入远超容量的填充数据，再写入带 usage 的末块。
	filler := bytes.Repeat([]byte("x"), 8000)
	for i := 0; i < 10; i++ {
		tb.Write(filler)
	}
	tb.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	if len(tb.buf) > tailCap {
		t.Fatalf("尾部缓冲超过容量: %d", len(tb.buf))
	}
	prompt, completion, ok := parseUsageTail(tb.buf)
	if !ok || prompt != 3 || completion != 4 {
		t.Fatalf("滚动后提取失败: prompt=%v completion=%v ok=%v", prompt, completion, ok)
	}

	// 单次写入超过容量：只保留末尾。
	var tb2 tailBuffer
	big := append(bytes.Repeat([]byte("y"), tailCap), []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`)...)
	tb2.Write(big)
	if len(tb2.buf) != tailCap {
		t.Fatalf("单次超容量写入后长度应为 %d: %d", tailCap, len(tb2.buf))
	}
	if _, _, ok := parseUsageTail(tb2.buf); !ok {
		t.Fatal("末尾 usage 应保留")
	}
}
