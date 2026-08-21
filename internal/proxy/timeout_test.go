// H2 修复验证：非流式响应体读取的"预算型"超时。
//
// 背景：http.Client 不设整体超时（proxy.go），transport 的 ResponseHeaderTimeout
// 只覆盖"等响应头"；响应头到达后，非流式的 body 读取此前无任何期限，上游回 200
// 头后僵死会永久挂起 goroutine/连接/并发槽位。
//
// 修复：非流式请求（上游响应非 SSE）在转发循环套 deadlineReader，预算 =
// UpstreamResponseTimeout 剩余时间、下限 30s；预算到期以超时错误退出读取循环。
// 流式（SSE 长流）不受此限制。
package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"ai-gateway/internal/config"
)

// upstreamStreamErrors 读回指定上游的"stream"类错误计数。CounterVec 元素实现
// Metric.Write，直接用 dto 读值，避免引入 testutil 子包（会要求动 go.mod）。
func upstreamStreamErrors(st *stack, backendID string) float64 {
	var m dto.Metric
	_ = st.handler.gw.UpstreamErrors.WithLabelValues(backendID, "stream").Write(&m)
	return m.GetCounter().GetValue()
}

// TestNonStreamingBodyReadTimeout 验证非流式请求在响应头到达后的 body 读取有
// 预算期限：上游写 200 头后挂起（永不发 body），配置极短 UpstreamResponseTimeout
// （50ms）——预算按 30s 下限兜底——断言请求在预算内以"body 读取超时"结束，
// 且不会无限挂起。
func TestNonStreamingBodyReadTimeout(t *testing.T) {
	block := make(chan struct{}) // 永不关闭 → 上游 handler 永不返回、body 永不发
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block
	}))
	defer upstream.Close()
	defer close(block) // LIFO：先放行卡死的 handler，再关服务器（否则 Close 等待 handler 永不返回）

	st := newStack(t, &config.Config{
		Server:   config.ServerConfig{UpstreamResponseTimeout: config.Duration(50 * time.Millisecond)},
		Backends: []config.BackendConfig{{ID: "b1", URL: upstream.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.handler.ServeHTTP(rec, req)
	}()

	// 预算下限 30s：请求应在预算到期后不久返回，而不是无限挂起。
	// 40s 上限 = 30s 预算 + CI 余量，仅用于兜底超时断言。
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("非流式请求应在预算（30s 下限）内超时失败，不能无限挂起")
	}

	// 200 头已透传（写头时尚未超时），body 读取超时后 ServeHTTP 返回。
	if rec.Code != http.StatusOK {
		t.Fatalf("上游 200 头应先透传，实际 %d", rec.Code)
	}
	// 超时被记录为一次上游流错误（body_read_timeout）。
	if got := upstreamStreamErrors(st, "b1"); got != 1 {
		t.Fatalf("body 读取超时应记为 1 次上游流错误，实际 %v", got)
	}
}

// TestStreamingNotSubjectToBodyBudget 守卫：SSE 流式请求不受非流式读超时约束——
// 即使 UpstreamResponseTimeout 极短（50ms），分块间隔远大于该值的长流也能完整送达。
func TestStreamingNotSubjectToBodyBudget(t *testing.T) {
	const chunkInterval = 100 * time.Millisecond // 远大于 50ms 响应超时配置
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		for i := 0; i < 4; i++ {
			time.Sleep(chunkInterval)
			write("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n")
		}
		write("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	st := newStack(t, &config.Config{
		Server:   config.ServerConfig{UpstreamResponseTimeout: config.Duration(50 * time.Millisecond)},
		Backends: []config.BackendConfig{{ID: "b1", URL: upstream.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", true)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("流式请求应 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 SSE，实际 %q", ct)
	}
	body := rec.Body.String()
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("流式响应应完整送达并含 [DONE] 收尾，实际出现 %d 次", got)
	}
	// 流式不受预算约束 → 不应产生上游流错误。
	if got := upstreamStreamErrors(st, "b1"); got != 0 {
		t.Fatalf("流式请求不应产生上游流错误，实际 %v", got)
	}
}
