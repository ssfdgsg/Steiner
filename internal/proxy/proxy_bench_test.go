// 转发链路基准：mock 后端上的端到端单请求耗时（含本机回环 HTTP 往返），
// 反映网关自身开销（读体/特征提取/限流/选路/转发/回写）的上界。
package proxy

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/test/mockbackend"
)

func BenchmarkForwardE2E(b *testing.B) {
	mock := mockbackend.New("vllm")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(b, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}, Strategy: "least_request"}},
	})
	body := chatBody("m1", false)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			st.handler.ServeHTTP(rec, req)
			if rec.Code != 200 {
				b.Fatalf("期望 200，实际 %d", rec.Code)
			}
		}
	})
}
