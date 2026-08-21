package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"ai-gateway/internal/config"
)

const (
	largeRequestContentBytes = 16 << 20
	largeQueryBytes          = 128 << 10
	largeResponseBytes       = 32 << 20
	largeSSEContentBytes     = 16 << 20
	largeChunkBytes          = 32 << 10
)

// TestLargeRequestBodyAndQueryForward 验证大 prompt 与长 URL query 不被截断或改写。
// 请求体上限精确设置为本次 body 长度，同时覆盖“刚好等于上限应放行”的边界。
func TestLargeRequestBodyAndQueryForward(t *testing.T) {
	content := strings.Repeat("Q", largeRequestContentBytes)
	body, err := json.Marshal(map[string]any{
		"model":  "m1",
		"stream": false,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
	})
	if err != nil {
		t.Fatalf("构造大请求失败: %v", err)
	}
	wantBodyHash := sha256.Sum256(body)
	queryValue := strings.Repeat("q", largeQueryBytes)
	wantRawQuery := "blob=" + queryValue

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != wantRawQuery {
			http.Error(w, fmt.Sprintf("query 不完整: got=%d want=%d", len(r.URL.RawQuery), len(wantRawQuery)), http.StatusBadRequest)
			return
		}
		gotBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "读取 body 失败: "+readErr.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(gotBody)) != r.ContentLength || len(gotBody) != len(body) {
			http.Error(w, fmt.Sprintf("body 长度不符: got=%d content-length=%d want=%d",
				len(gotBody), r.ContentLength, len(body)), http.StatusBadRequest)
			return
		}
		if gotHash := sha256.Sum256(gotBody); gotHash != wantBodyHash {
			http.Error(w, "body SHA-256 不符", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"large-request-ok","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	st := newStack(t, &config.Config{
		Server:   config.ServerConfig{MaxBodyBytes: int64(len(body))},
		Backends: []config.BackendConfig{{ID: "b1", URL: upstream.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions?"+wantRawQuery, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("大请求转发失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Upstream-Backend"); got != "b1" {
		t.Fatalf("后端标识不符: %q", got)
	}
}

// TestLargeNonStreamingResponse 验证 32MiB 非流式响应逐字节透传，且 usage 位于
// 大响应尾部时仍能被固定容量 tailBuffer 正确提取。
func TestLargeNonStreamingResponse(t *testing.T) {
	prefix := []byte(`{"id":"large","choices":[{"message":{"content":"`)
	chunk := bytes.Repeat([]byte("R"), largeChunkBytes)
	repeats := largeResponseBytes / len(chunk)
	suffix := []byte(`"}}],"usage":{"prompt_tokens":123,"completion_tokens":456}}`)
	wantLen := len(prefix) + repeats*len(chunk) + len(suffix)
	wantHash := repeatedPayloadHash(prefix, chunk, repeats, suffix)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Large-Response", "non-stream")
		w.Header().Set("Content-Length", strconv.Itoa(wantLen))
		_, _ = w.Write(prefix)
		for i := 0; i < repeats; i++ {
			_, _ = w.Write(chunk)
		}
		_, _ = w.Write(suffix)
	}))
	defer upstream.Close()

	st := newLargeResponseStack(t, upstream.URL)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	assertLargeResponse(t, rec, wantLen, wantHash)
	if got := rec.Header().Get("X-Large-Response"); got != "non-stream" {
		t.Fatalf("上游响应头未透传: %q", got)
	}
	a, err := st.handler.gw.Aggregate()
	if err != nil {
		t.Fatalf("聚合 usage 指标失败: %v", err)
	}
	if a.PromptTokens != 123 || a.CompletionTokens != 456 {
		t.Fatalf("大响应尾部 usage 未正确记账: prompt=%v completion=%v",
			a.PromptTokens, a.CompletionTokens)
	}
}

// TestLargeStreamingResponse 验证约 16MiB SSE 分帧在频繁 Flush 下完整透传，
// 并保留 usage 尾块与 [DONE] 收尾。
func TestLargeStreamingResponse(t *testing.T) {
	framePrefix := []byte(`data: {"choices":[{"delta":{"content":"`)
	frameContent := bytes.Repeat([]byte("S"), largeChunkBytes)
	frameSuffix := []byte(`"}}]}` + "\n\n")
	repeats := largeSSEContentBytes / len(frameContent)
	usageTail := []byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":321,\"completion_tokens\":789}}\n\n" +
		"data: [DONE]\n\n")
	frame := make([]byte, 0, len(framePrefix)+len(frameContent)+len(frameSuffix))
	frame = append(frame, framePrefix...)
	frame = append(frame, frameContent...)
	frame = append(frame, frameSuffix...)
	wantLen := repeats*len(frame) + len(usageTail)
	wantHash := repeatedPayloadHash(nil, frame, repeats, usageTail)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Large-Response", "sse")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < repeats; i++ {
			_, _ = w.Write(frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write(usageTail)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	st := newLargeResponseStack(t, upstream.URL)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", true)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	assertLargeResponse(t, rec, wantLen, wantHash)
	if !rec.Flushed {
		t.Fatal("大 SSE 响应未触发 Flush")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type 不符: %q", ct)
	}
	if !bytes.HasSuffix(rec.Body.Bytes(), usageTail) {
		t.Fatal("大 SSE 响应缺少 usage 或 [DONE] 尾部")
	}
	a, err := st.handler.gw.Aggregate()
	if err != nil {
		t.Fatalf("聚合 SSE usage 指标失败: %v", err)
	}
	if a.PromptTokens != 321 || a.CompletionTokens != 789 {
		t.Fatalf("大 SSE 尾部 usage 未正确记账: prompt=%v completion=%v",
			a.PromptTokens, a.CompletionTokens)
	}
}

func newLargeResponseStack(t *testing.T, upstreamURL string) *stack {
	t.Helper()
	return newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: upstreamURL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
}

func repeatedPayloadHash(prefix, chunk []byte, repeats int, suffix []byte) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write(prefix)
	for i := 0; i < repeats; i++ {
		_, _ = h.Write(chunk)
	}
	_, _ = h.Write(suffix)
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func assertLargeResponse(t *testing.T, rec *httptest.ResponseRecorder, wantLen int, wantHash [sha256.Size]byte) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("大响应转发失败: code=%d", rec.Code)
	}
	if got := rec.Body.Len(); got != wantLen {
		t.Fatalf("大响应长度不符: got=%d want=%d", got, wantLen)
	}
	if gotHash := sha256.Sum256(rec.Body.Bytes()); gotHash != wantHash {
		t.Fatalf("大响应 SHA-256 不符: got=%x want=%x", gotHash, wantHash)
	}
}

// BenchmarkLargeRequestForward16MiB 测量无需 KV/prompt 特征时，16MiB 请求经轻量
// 信封扫描后原样转发到本机 mock 上游的端到端成本。
func BenchmarkLargeRequestForward16MiB(b *testing.B) {
	body, _ := json.Marshal(map[string]any{
		"model": "m1", "stream": false,
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("Q", 16<<20)}},
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	st := newStack(b, &config.Config{
		Server:   config.ServerConfig{MaxBodyBytes: int64(len(body))},
		Backends: []config.BackendConfig{{ID: "b1", URL: upstream.URL, Engine: "vllm"}},
		Models: []config.ModelRoute{{
			Name: "m1", Backends: []string{"b1"}, Strategy: "least_request",
		}},
	})

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("大请求转发失败: %d %s", rec.Code, rec.Body.String())
		}
	}
}
