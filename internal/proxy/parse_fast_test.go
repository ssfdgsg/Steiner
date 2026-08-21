package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/scheduler"
)

func TestScanRequestMetaMatchesJSONDecoder(t *testing.T) {
	cases := []string{
		`{"model":"m1","stream":true,"priority":2.5,"user":"u","messages":[{"content":"x}\\\"y"}]}`,
		` { "messages": [{"role":"user","content":["a",{"nested":[1,true,null]}]}], "user":"u2", "model":"m2", "stream":false } `,
		`{"mo\u0064el":"escaped","unknown":{"deep":[{"x":"[}]"}]},"priority":-1.25}`,
	}
	for _, raw := range cases {
		var want requestMeta
		if err := json.Unmarshal([]byte(raw), &want); err != nil {
			t.Fatalf("测试 JSON 非法: %v", err)
		}
		var got requestMeta
		if !scanRequestMeta([]byte(raw), &got) {
			t.Fatalf("轻量扫描失败: %s", raw)
		}
		if got != want {
			t.Fatalf("轻量扫描与标准解码不一致: got=%+v want=%+v", got, want)
		}
	}
}

func TestParseRequestMetaSkipsLargePrompt(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "m1", "stream": true, "priority": 2.5, "user": "body-user",
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("Q", 1<<20)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Session-Id", "header-user")
	req := parseRequestMeta(r, body)
	if req.Model != "m1" || !req.Stream || req.Priority != 2.5 || req.SessionID != "header-user" {
		t.Fatalf("轻量信封字段错误: %+v", req)
	}
	if req.PromptText != "" || req.PromptLen != 0 || req.PromptTokensEst != 0 {
		t.Fatalf("轻量信封不应构造 prompt 特征: %+v", req)
	}
}

func TestPopulatePromptFeaturesLimit(t *testing.T) {
	const contentBytes = 1 << 20
	body, _ := json.Marshal(map[string]any{
		"model":    "m1",
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("P", contentBytes)}},
	})
	doc := parseRequestDocument(body)
	req := &scheduler.Request{}
	populatePromptFeatures(req, doc, 4096)

	wantLen := len("user:") + contentBytes + 1 // role + content + 换行
	if req.PromptLen != wantLen {
		t.Fatalf("完整 prompt 长度统计错误: got=%d want=%d", req.PromptLen, wantLen)
	}
	if len(req.PromptText) != 4096 {
		t.Fatalf("缓存文本应限制到 4096 字节: got=%d", len(req.PromptText))
	}
	if req.PromptTokensEst != wantLen/4 {
		t.Fatalf("token 估算应基于完整长度: got=%d want=%d", req.PromptTokensEst, wantLen/4)
	}

	populatePromptFeatures(req, doc, 0)
	if req.PromptText != "" || req.PromptLen != wantLen {
		t.Fatalf("零拷贝模式应只保留长度: text=%d len=%d", len(req.PromptText), req.PromptLen)
	}
}

func TestReadRequestBodyAllocationPaths(t *testing.T) {
	t.Run("known length", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/", strings.NewReader("payload"))
		body, tooLarge, err := readRequestBody(r, 7)
		if err != nil || tooLarge || string(body) != "payload" {
			t.Fatalf("精确长度读取失败: body=%q tooLarge=%v err=%v", body, tooLarge, err)
		}
	})
	t.Run("declared too large", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/", strings.NewReader("payload"))
		if _, tooLarge, err := readRequestBody(r, 6); err != nil || !tooLarge {
			t.Fatalf("已知超限应立即拒绝: tooLarge=%v err=%v", tooLarge, err)
		}
	})
	t.Run("chunked too large", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader("payload")))
		r.ContentLength = -1
		if _, tooLarge, err := readRequestBody(r, 6); err != nil || !tooLarge {
			t.Fatalf("未知长度超限应拒绝: tooLarge=%v err=%v", tooLarge, err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/", strings.NewReader("short"))
		r.ContentLength = 8
		if _, _, err := readRequestBody(r, 8); err == nil {
			t.Fatal("声明长度大于实际 body 应返回读取错误")
		}
	})
}

// BenchmarkParseLargeRequest 对照原完整解析和无需 prompt 特征时的轻量信封扫描。
func BenchmarkParseLargeRequest(b *testing.B) {
	body, _ := json.Marshal(map[string]any{
		"model": "m1", "stream": false,
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("Q", 16<<20)}},
	})
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))

	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			req, doc := parseRequest(r, body)
			if req.Model == "" || doc == nil {
				b.Fatal("完整解析失败")
			}
		}
	})
	b.Run("meta_only", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			if req := parseRequestMeta(r, body); req.Model == "" {
				b.Fatal("轻量解析失败")
			}
		}
	})
}
