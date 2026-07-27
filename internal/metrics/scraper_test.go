// 指标抓取器单测：vllm/sglang 指标解析、counter 速率派生、采集失败保留旧值。
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

func newScrapeBackend(t *testing.T, url, engine string) *backend.Backend {
	t.Helper()
	b, err := backend.New(config.BackendConfig{
		ID: "b1", URL: url, Engine: engine, Weight: 1, MetricsPath: "/metrics",
	})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	return b
}

func TestScrapeVLLM(t *testing.T) {
	var counter atomic.Int64
	counter.Store(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"a\"} 3\nvllm:num_requests_running{model_name=\"b\"} 2\n")
		fmt.Fprintf(w, "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting 7\n")
		fmt.Fprintf(w, "# TYPE vllm:gpu_cache_usage_perc gauge\nvllm:gpu_cache_usage_perc 0.42\n")
		fmt.Fprintf(w, "# TYPE vllm:gpu_prefix_cache_hit_rate gauge\nvllm:gpu_prefix_cache_hit_rate 0.66\n")
		fmt.Fprintf(w, "# TYPE vllm:generation_tokens_total counter\nvllm:generation_tokens_total %d\n", counter.Load())
	}))
	defer srv.Close()

	b := newScrapeBackend(t, srv.URL, "vllm")
	s := NewScraper(func() []*backend.Backend { return []*backend.Backend{b} }, time.Second, time.Second)

	s.ScrapeOnce(context.Background(), b)
	snap := b.Snapshot()
	if snap.Err != "" {
		t.Fatalf("采集不应失败: %s", snap.Err)
	}
	if snap.Running != 5 { // 多序列求和 3+2
		t.Fatalf("Running 期望 5，实际 %v", snap.Running)
	}
	if snap.Waiting != 7 || snap.KVUsage != 0.42 || snap.HitRate != 0.66 {
		t.Fatalf("字段解析错误: %+v", snap)
	}
	if snap.GenTokPerSec != 0 {
		t.Fatalf("首轮无历史，速率应为 0，实际 %v", snap.GenTokPerSec)
	}

	// 第二轮：counter 增长 → 速率派生 > 0。
	counter.Store(2000)
	time.Sleep(30 * time.Millisecond)
	s.ScrapeOnce(context.Background(), b)
	snap = b.Snapshot()
	if snap.GenTokPerSec <= 0 {
		t.Fatalf("counter 增长后吞吐速率应大于 0，实际 %v", snap.GenTokPerSec)
	}
	if _, ok := snap.Raw["rate:vllm:generation_tokens_total"]; !ok {
		t.Fatal("Raw 中应含速率派生值")
	}
}

func TestScrapeSGLang(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE sglang:num_running_reqs gauge\nsglang:num_running_reqs 4\n")
		fmt.Fprint(w, "# TYPE sglang:num_queue_reqs gauge\nsglang:num_queue_reqs 9\n")
		fmt.Fprint(w, "# TYPE sglang:token_usage gauge\nsglang:token_usage 0.31\n")
		fmt.Fprint(w, "# TYPE sglang:cache_hit_rate gauge\nsglang:cache_hit_rate 0.5\n")
		fmt.Fprint(w, "# TYPE sglang:gen_throughput gauge\nsglang:gen_throughput 123.4\n")
	}))
	defer srv.Close()

	b := newScrapeBackend(t, srv.URL, "sglang")
	s := NewScraper(func() []*backend.Backend { return []*backend.Backend{b} }, time.Second, time.Second)
	s.ScrapeOnce(context.Background(), b)

	snap := b.Snapshot()
	if snap.Running != 4 || snap.Waiting != 9 || snap.KVUsage != 0.31 {
		t.Fatalf("sglang 字段解析错误: %+v", snap)
	}
	if snap.GenTokPerSec != 123.4 {
		t.Fatalf("sglang 吞吐应直读 gauge，期望 123.4 实际 %v", snap.GenTokPerSec)
	}
}

// TestScrapeOmniReusesFamily omni 变体复用基础引擎的指标映射表。
func TestScrapeOmniReusesFamily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 6\n")
	}))
	defer srv.Close()

	b := newScrapeBackend(t, srv.URL, "vllm_omni")
	s := NewScraper(func() []*backend.Backend { return []*backend.Backend{b} }, time.Second, time.Second)
	s.ScrapeOnce(context.Background(), b)
	if got := b.Snapshot().Running; got != 6 {
		t.Fatalf("vllm_omni 应复用 vllm 映射，Running 期望 6 实际 %v", got)
	}
}

func TestScrapeFailureKeepsOldValues(t *testing.T) {
	fail := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 8\n")
	}))
	defer srv.Close()

	b := newScrapeBackend(t, srv.URL, "vllm")
	s := NewScraper(func() []*backend.Backend { return []*backend.Backend{b} }, time.Second, time.Second)
	s.ScrapeOnce(context.Background(), b)
	if b.Snapshot().Running != 8 {
		t.Fatal("首轮采集应成功")
	}

	fail.Store(true)
	s.ScrapeOnce(context.Background(), b)
	snap := b.Snapshot()
	if snap.Err == "" {
		t.Fatal("采集失败应记录错误")
	}
	if snap.Running != 8 {
		t.Fatalf("采集失败应保留旧值 8，实际 %v", snap.Running)
	}
}

func TestHitRateFromCounters(t *testing.T) {
	queries := atomic.Int64{}
	hits := atomic.Int64{}
	queries.Store(100)
	hits.Store(10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 新版 vllm 只暴露 queries/hits counter，无 hit_rate gauge。
		fmt.Fprintf(w, "# TYPE vllm:gpu_prefix_cache_queries_total counter\nvllm:gpu_prefix_cache_queries_total %d\n", queries.Load())
		fmt.Fprintf(w, "# TYPE vllm:gpu_prefix_cache_hits_total counter\nvllm:gpu_prefix_cache_hits_total %d\n", hits.Load())
	}))
	defer srv.Close()

	b := newScrapeBackend(t, srv.URL, "vllm")
	s := NewScraper(func() []*backend.Backend { return []*backend.Backend{b} }, time.Second, time.Second)
	s.ScrapeOnce(context.Background(), b)

	queries.Store(200) // +100 次查询
	hits.Store(90)     // +80 次命中 → 瞬时命中率 0.8
	time.Sleep(20 * time.Millisecond)
	s.ScrapeOnce(context.Background(), b)
	got := b.Snapshot().HitRate
	if got < 0.79 || got > 0.81 {
		t.Fatalf("由 counter 对推导的命中率期望约 0.8，实际 %v", got)
	}
}
