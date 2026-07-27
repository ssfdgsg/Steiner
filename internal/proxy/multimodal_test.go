// 多模态特征提取与容量准入单测。
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/queue"
	"ai-gateway/test/mockbackend"
)

// mmBody 构造带图像分段的聊天请求体。
func mmBody(imageURL string) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"model": "m1",
		"messages": []map[string]interface{}{
			{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "这张图里有什么"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]string{"url": imageURL}},
			}},
		},
	})
	return b
}

// TestParseMultimodalFeatures 验证多模态计数、token 估算与哈希占位。
func TestParseMultimodalFeatures(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req, _ := parseRequest(r, mmBody("https://example.com/cat.png"))

	if !req.IsMultimodal || req.ImageCount != 1 || req.AudioCount != 0 {
		t.Fatalf("多模态特征不符: multimodal=%v image=%d audio=%d",
			req.IsMultimodal, req.ImageCount, req.AudioCount)
	}
	if req.PromptTokensEst < imageTokenWeight {
		t.Fatalf("token 估算应包含图像权重: %d", req.PromptTokensEst)
	}
	if !strings.Contains(req.PromptText, "[img:") {
		t.Fatalf("前缀文本应含图像哈希占位: %q", req.PromptText)
	}

	// 相同图片 → 相同占位（前缀可命中）；不同图片 → 不同占位。
	req2, _ := parseRequest(r, mmBody("https://example.com/cat.png"))
	if req.PromptText != req2.PromptText {
		t.Fatal("相同素材的请求应产生相同前缀文本")
	}
	req3, _ := parseRequest(r, mmBody("https://example.com/dog.png"))
	if req.PromptText == req3.PromptText {
		t.Fatal("不同素材的请求不应产生相同前缀文本")
	}

	// 纯文本请求不受影响。
	req4, _ := parseRequest(r, chatBody("m1", false))
	if req4.IsMultimodal || req4.ImageCount != 0 {
		t.Fatalf("纯文本请求不应有多模态特征: %+v", req4)
	}
}

// TestAdmissionRejects 验证容量准入：将要排队且表达式为 false 时立即 429，
// 而非排队到超时；不触发排队路径（有容量）时准入不生效。
func TestAdmissionRejects(t *testing.T) {
	mock := mockbackend.New("vllm")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Queue:    config.QueueConfig{Enabled: true, MaxDepth: 8, MaxWait: config.Duration(5e9)},
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	st.handler.SetQueue(queue.New(8, 5*time.Second))
	prog, err := policy.CompileBool("prompt_tokens_est < 5 && available_count > 0")
	if err != nil {
		t.Fatalf("编译准入表达式失败: %v", err)
	}
	st.handler.SetAdmission(prog, "prompt_tokens_est < 5 && available_count > 0")

	// 有容量：准入不参与，正常转发。
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader(chatBody("m1", false))))
	if rec.Code != 200 {
		t.Fatalf("有容量时应正常转发: %d", rec.Code)
	}

	// 隔离后端制造"无可用容量"：请求走排队路径，被准入立即拒绝（不等 5s 超时）。
	st.reg.Get("b1").Cordon(true)
	rec = httptest.NewRecorder()
	st.handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader(chatBody("m1", false))))
	if rec.Code != 429 {
		t.Fatalf("准入拒绝应返回 429: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "容量准入拒绝") {
		t.Fatalf("应返回准入拒绝信息: %s", rec.Body.String())
	}
}
