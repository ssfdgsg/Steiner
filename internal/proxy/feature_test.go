// 会话粘性存储与容量排队在代理层的行为测试。
package proxy

import (
	"bytes"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/session"
	"ai-gateway/test/mockbackend"
)

// TestSessionStoreSticky 启用粘性存储后，round_robin 也应被粘性短路。
func TestSessionStoreSticky(t *testing.T) {
	m1 := mockbackend.New("vllm")
	s1 := httptest.NewServer(m1.Handler())
	defer s1.Close()
	m2 := mockbackend.New("vllm")
	s2 := httptest.NewServer(m2.Handler())
	defer s2.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: s1.URL, Engine: "vllm"},
			{ID: "b2", URL: s2.URL, Engine: "vllm"},
		},
		// 轮询策略本会交替选路，用于验证粘性短路生效。
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}, Strategy: "round_robin"}},
	})
	st.handler.SetSessionStore(session.NewStore(time.Minute, 1000))

	var firstBackend string
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		req.Header.Set("X-Session-Id", "sticky-1")
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("期望 200，实际 %d", rec.Code)
		}
		got := rec.Header().Get("X-Upstream-Backend")
		if firstBackend == "" {
			firstBackend = got
		} else if got != firstBackend {
			t.Fatalf("粘性会话应固定在 %s，第 %d 次却路由到 %s", firstBackend, i, got)
		}
	}
}

// TestSessionRebindOnUnhealthy 粘住的后端下线后应改绑新后端而非失败。
func TestSessionRebindOnUnhealthy(t *testing.T) {
	m1 := mockbackend.New("vllm")
	s1 := httptest.NewServer(m1.Handler())
	defer s1.Close()
	m2 := mockbackend.New("vllm")
	s2 := httptest.NewServer(m2.Handler())
	defer s2.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: s1.URL, Engine: "vllm"},
			{ID: "b2", URL: s2.URL, Engine: "vllm"},
		},
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}, Strategy: "round_robin"}},
	})
	st.handler.SetSessionStore(session.NewStore(time.Minute, 1000))

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req.Header.Set("X-Session-Id", "sticky-2")
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	bound := rec.Header().Get("X-Upstream-Backend")

	st.reg.Get(bound).SetHealthy(false) // 粘住的后端下线

	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req2.Header.Set("X-Session-Id", "sticky-2")
	rec2 := httptest.NewRecorder()
	st.handler.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("绑定后端下线后应改绑成功，实际 %d", rec2.Code)
	}
	if got := rec2.Header().Get("X-Upstream-Backend"); got == bound {
		t.Fatalf("不应继续路由到已下线的 %s", bound)
	}
}

// TestQueueWaitsForCapacity 并发额度占满时请求应排队等待而非立即失败。
func TestQueueWaitsForCapacity(t *testing.T) {
	slow := mockbackend.New("vllm")
	slow.SetFault(mockbackend.Fault{TTFTDelayMS: 200}) // 慢后端占住唯一额度
	srv := httptest.NewServer(slow.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm", MaxConcurrency: 1}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	st.handler.SetQueue(queue.New(16, 2*time.Second))

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
			rec := httptest.NewRecorder()
			st.handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
		time.Sleep(30 * time.Millisecond) // 保证第一个请求先占住额度
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("两个请求都应成功（第二个经排队），实际 %v", codes)
	}
}

// TestQueueTimeout 容量长期不释放时排队应超时返回 503。
func TestQueueTimeout(t *testing.T) {
	slow := mockbackend.New("vllm")
	slow.SetFault(mockbackend.Fault{TTFTDelayMS: 1000})
	srv := httptest.NewServer(slow.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm", MaxConcurrency: 1}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	st.handler.SetQueue(queue.New(16, 100*time.Millisecond))

	go func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		st.handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(30 * time.Millisecond)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("排队超时应 503，实际 %d", rec.Code)
	}
}
