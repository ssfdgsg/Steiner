package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

// TestBackendCollector_透出快照 验证后端归一化指标经 Collector 正确透出。
func TestBackendCollector_透出快照(t *testing.T) {
	b, err := backend.New(config.BackendConfig{
		ID: "b1", URL: "http://127.0.0.1:8000", Engine: "vllm", Weight: 1,
	})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	b.SetSnapshot(&backend.Snapshot{
		Time: time.Now(), Running: 3, Waiting: 7, KVUsage: 0.5, HitRate: 0.8, GenTokPerSec: 120,
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewBackendCollector(func() []*backend.Backend { return []*backend.Backend{b} }))
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	got := map[string]float64{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			got[mf.GetName()] = m.GetGauge().GetValue()
		}
	}
	expect := map[string]float64{
		"gateway_backend_info":                  1,
		"gateway_backend_running_requests":      3,
		"gateway_backend_waiting_requests":      7,
		"gateway_backend_kv_cache_usage":        0.5,
		"gateway_backend_prefix_hit_rate":       0.8,
		"gateway_backend_gen_tokens_per_second": 120,
		"gateway_backend_scrape_up":             1,
	}
	for name, want := range expect {
		if got[name] != want {
			t.Fatalf("指标 %s 期望 %v 实际 %v（全部: %v）", name, want, got[name], got)
		}
	}

	// 采集出错时 scrape_up 应为 0。
	b.SetSnapshot(&backend.Snapshot{Time: time.Now(), Err: "连接拒绝"})
	families, _ = reg.Gather()
	for _, mf := range families {
		if mf.GetName() == "gateway_backend_scrape_up" {
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 0 {
				t.Fatalf("采集失败后 scrape_up 应为 0: %v", v)
			}
		}
	}
}

// TestNewGateway_指标注册完整 验证全部指标可注册且无重名冲突（MustRegister 不 panic）。
func TestNewGateway_指标注册完整(t *testing.T) {
	g := NewGateway()
	// 触发一次带标签写入，确认标签基数定义正确。
	g.PromptTokens.WithLabelValues("b1", "m1").Add(10)
	g.CompletionTokens.WithLabelValues("b1", "m1").Add(20)
	g.UpstreamErrors.WithLabelValues("b1", "connect").Inc()
	g.PickDuration.WithLabelValues("expression").Observe(0.0001)
	g.BuildInfo.WithLabelValues("test").Set(1)
	families, err := g.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range families {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"gateway_prompt_tokens_total", "gateway_completion_tokens_total",
		"gateway_upstream_errors_total", "gateway_pick_duration_seconds", "gateway_build_info",
	} {
		if !names[want] {
			t.Fatalf("缺少指标 %s（已注册: %v）", want, names)
		}
	}
}
