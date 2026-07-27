// 代理层单测：常规转发、SSE 流式、失败重试、限流、模型清单与边界条件。
// 后端使用 test/mockbackend 四引擎模拟器。
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/scheduler"
	"ai-gateway/test/mockbackend"
)

// stack 一套可用于测试的完整网关组件。
type stack struct {
	handler *Handler
	cfg     *config.Config
	reg     *backend.Registry
}

// newStack 按给定配置装配代理及其全部依赖（testing.TB：单测与基准共用）。
func newStack(t testing.TB, cfg *config.Config) *stack {
	t.Helper()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	pol := policy.NewEngine()
	for name, pc := range cfg.Policies {
		if err := pol.Set(name, pc.Filter, pc.Score); err != nil {
			t.Fatalf("编译策略失败: %v", err)
		}
	}
	var tree *kvcache.Tree
	if cfg.KVCache.Enabled {
		tree = kvcache.NewTree(cfg.KVCache.MaxPrefixBytes, cfg.KVCache.TTL.D())
	}
	sched := scheduler.New(pol, tree, cfg.KVCache)
	pdMgr, err := pd.NewManager(cfg, reg)
	if err != nil {
		t.Fatalf("构建 PD 管理器失败: %v", err)
	}
	gw := metrics.NewGateway()
	return &stack{handler: New(cfg, reg, sched, pdMgr, gw), cfg: cfg, reg: reg}
}

// chatBody 构造聊天补全请求体。
func chatBody(model string, stream bool) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"model":  model,
		"stream": stream,
		"messages": []map[string]interface{}{
			{"role": "system", "content": "你是助手"},
			{"role": "user", "content": "你好"},
		},
	})
	return b
}

func TestForwardBasic(t *testing.T) {
	mock := mockbackend.New("vllm")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Upstream-Backend"); got != "b1" {
		t.Fatalf("X-Upstream-Backend 期望 b1，实际 %q", got)
	}
	if !strings.Contains(rec.Body.String(), "模拟应答") {
		t.Fatalf("响应体不符: %s", rec.Body.String())
	}
	if got := mock.LastBody()["model"]; got != "m1" {
		t.Fatalf("后端收到的 model 不符: %v", got)
	}
}

func TestForwardSSEStream(t *testing.T) {
	mock := mockbackend.New("sglang")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "sglang"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", true)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 SSE，实际 %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data:") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("SSE 流不完整: %s", body)
	}
	if !rec.Flushed {
		t.Fatal("流式响应应逐块刷出")
	}
}

func TestRetryOnDeadBackend(t *testing.T) {
	mock := mockbackend.New("vllm")
	live := httptest.NewServer(mock.Handler())
	defer live.Close()
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close() // 拒连后端

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "dead", URL: deadURL, Engine: "vllm"},
			{ID: "live", URL: live.URL, Engine: "vllm"},
		},
		// least_request 在负载相同时取列表第一个（dead），必然触发重试换后端。
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"dead", "live"}, Strategy: "least_request"}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("重试后应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Upstream-Backend"); got != "live" {
		t.Fatalf("应换到 live 后端，实际 %q", got)
	}
}

func TestRetryOn503(t *testing.T) {
	bad := mockbackend.New("vllm")
	bad.SetFault(mockbackend.Fault{FailCode: 503})
	badSrv := httptest.NewServer(bad.Handler())
	defer badSrv.Close()
	good := mockbackend.New("vllm")
	goodSrv := httptest.NewServer(good.Handler())
	defer goodSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "bad", URL: badSrv.URL, Engine: "vllm"},
			{ID: "good", URL: goodSrv.URL, Engine: "vllm"},
		},
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"bad", "good"}, Strategy: "least_request"}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("X-Upstream-Backend") != "good" {
		t.Fatalf("503 应触发换后端重试，实际 code=%d backend=%q", rec.Code, rec.Header().Get("X-Upstream-Backend"))
	}
}

func Test4xxPassthroughNoRetry(t *testing.T) {
	bad := mockbackend.New("vllm")
	bad.SetFault(mockbackend.Fault{FailCode: 400})
	badSrv := httptest.NewServer(bad.Handler())
	defer badSrv.Close()
	good := mockbackend.New("vllm")
	goodSrv := httptest.NewServer(good.Handler())
	defer goodSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "bad", URL: badSrv.URL, Engine: "vllm"},
			{ID: "good", URL: goodSrv.URL, Engine: "vllm"},
		},
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"bad", "good"}, Strategy: "least_request"}},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("4xx 是请求问题，应原样透传不重试，实际 %d", rec.Code)
	}
	if good.ReqCount() != 0 {
		t.Fatal("4xx 不应触发换后端")
	}
}

func TestRateLimit(t *testing.T) {
	mock := mockbackend.New("vllm")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm"}},
		Models: []config.ModelRoute{{
			Name: "m1", Backends: []string{"b1"}, RateLimitQPS: 0.5, RateLimitBurst: 1,
		}},
	})

	req1 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec1 := httptest.NewRecorder()
	st.handler.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("首个请求应放行，实际 %d", rec1.Code)
	}
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec2 := httptest.NewRecorder()
	st.handler.ServeHTTP(rec2, req2)
	if rec2.Code != 429 {
		t.Fatalf("突发额度耗尽应 429，实际 %d", rec2.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"}},
		Models: []config.ModelRoute{
			{Name: "m1", Backends: []string{"b1"}},
			{Name: "m2", Backends: []string{"b1"}},
			{Name: "*", Backends: []string{"b1"}},
		},
	})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Data) != 2 { // "*" 不对外展示
		t.Fatalf("模型清单期望 2 项，实际 %+v", out.Data)
	}
}

func TestNoRouteReturns404(t *testing.T) {
	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("unknown", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("无路由且无兜底应 404，实际 %d", rec.Code)
	}
}

func TestBodyTooLarge(t *testing.T) {
	st := newStack(t, &config.Config{
		Server:   config.ServerConfig{MaxBodyBytes: 64},
		Backends: []config.BackendConfig{{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("超限请求体应 413，实际 %d", rec.Code)
	}
}

func TestSessionHeaderAffinity(t *testing.T) {
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
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}, Strategy: "consistent_hash"}},
	})

	var firstBackend string
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		req.Header.Set("X-Session-Id", "session-abc")
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("期望 200，实际 %d", rec.Code)
		}
		got := rec.Header().Get("X-Upstream-Backend")
		if firstBackend == "" {
			firstBackend = got
		} else if got != firstBackend {
			t.Fatalf("同一会话应粘住 %s，实际 %s", firstBackend, got)
		}
	}
}

// waitFor 轮询等待条件成立（用于异步断言）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
