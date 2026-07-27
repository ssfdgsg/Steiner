// 分布式追踪单测：用内存导出器验证每请求的 span 链结构、
// TTFT 事件、traceparent 上游传播与响应头关联。
package proxy

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"ai-gateway/internal/config"
	"ai-gateway/test/mockbackend"
)

// setupTestTracing 安装内存导出的全局 TracerProvider，测试结束恢复。
func setupTestTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	// 生产路径由 tracing.Setup 注册传播器，测试同样需要显式注册。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(old)
		otel.SetTextMapPropagator(oldProp)
	})
	return exp
}

// TestTraceSpanChain 验证一次成功转发产生 根 span + pick + forward 三级链，
// forward 含 first_byte（TTFT）事件，且共享同一 trace ID。
func TestTraceSpanChain(t *testing.T) {
	exp := setupTestTracing(t)

	mock := mockbackend.New("vllm")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Trace-Id") == "" || rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("响应应携带 X-Trace-Id 与 X-Request-Id")
	}

	spans := exp.GetSpans()
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	for _, want := range []string{"POST /v1/chat/completions", "gateway.pick", "gateway.forward"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("缺少 span %q，实际: %v", want, names(spans))
		}
	}

	// 同一条 trace。
	traceID := byName["POST /v1/chat/completions"].SpanContext.TraceID()
	for _, s := range spans {
		if s.SpanContext.TraceID() != traceID {
			t.Fatalf("span %s 不在同一 trace", s.Name)
		}
	}
	if rec.Header().Get("X-Trace-Id") != traceID.String() {
		t.Fatal("X-Trace-Id 应与根 span 的 trace ID 一致")
	}

	// forward span 应有 first_byte 事件（TTFT）。
	fwd := byName["gateway.forward"]
	hasTTFT := false
	for _, ev := range fwd.Events {
		if ev.Name == "first_byte" {
			hasTTFT = true
		}
	}
	if !hasTTFT {
		t.Fatalf("forward span 缺少 first_byte 事件: %+v", fwd.Events)
	}

	// 上游应收到 traceparent（W3C 传播）。
	if mock.LastHeader().Get("traceparent") == "" {
		t.Fatal("上游请求应携带 traceparent 头")
	}
}

// TestTraceRetrySpans 验证重试路径：两个 forward span（首个失败），根 span 仍成功。
func TestTraceRetrySpans(t *testing.T) {
	exp := setupTestTracing(t)

	bad := mockbackend.New("vllm")
	bad.SetFault(mockbackend.Fault{FailCode: 503})
	badSrv := httptest.NewServer(bad.Handler())
	defer badSrv.Close()
	good := httptest.NewServer(mockbackend.New("vllm").Handler())
	defer good.Close()

	st := newStack(t, &config.Config{
		Server: config.ServerConfig{Retries: 2},
		Backends: []config.BackendConfig{
			{ID: "bad", URL: badSrv.URL, Engine: "vllm"},
			{ID: "good", URL: good.URL, Engine: "vllm"},
		},
		// least_request：零负载平局时取池序首个（bad），保证首次必命中坏后端。
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"bad", "good"}, Strategy: "least_request"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("重试后应成功，状态码 %d", rec.Code)
	}

	forwards, picks := 0, 0
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "gateway.forward":
			forwards++
		case "gateway.pick":
			picks++
		}
	}
	// bad 命中一次失败 + good 成功一次 = 2 个 forward、2 次 pick。
	if forwards < 2 || picks < 2 {
		t.Fatalf("重试应产生多个 pick/forward span: picks=%d forwards=%d", picks, forwards)
	}
}

func names(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
