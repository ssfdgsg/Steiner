// 预设方案依赖的调度环境变量测试：ttft_ewma 反馈闭环与 preempt_rate 归一化。
package scheduler

import (
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/policy"
)

// TestLatencyFirstPresetPrefersFastBackend latency_first 预设应选 TTFT 更低的后端。
func TestLatencyFirstPresetPrefersFastBackend(t *testing.T) {
	slow, fast := newBackend(t, "slow"), newBackend(t, "fast")
	setLoad(slow, 1, 0, 0.3)
	setLoad(fast, 1, 0, 0.3)
	// 网关实测 TTFT：slow 800ms，fast 60ms。
	slow.ObserveTTFT(0.8)
	fast.ObserveTTFT(0.06)

	e := policy.NewEngine()
	pr := policy.FindPreset("latency_first")
	if pr == nil {
		t.Fatal("latency_first 预设应存在")
	}
	if err := e.Set("p", pr.Filter, pr.Score); err != nil {
		t.Fatalf("预设编译失败: %v", err)
	}
	s := New(e, nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "p"}
	route.SetPool([]*backend.Backend{slow, fast})

	for i := 0; i < 3; i++ {
		got, err := s.Pick(route, &Request{Model: "m"}, nil)
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if got.ID != "fast" {
			t.Fatalf("低时延方案应选 fast，实际 %s", got.ID)
		}
	}
}

// TestPreemptionSafePresetAvoidsPreempting preemption_safe 预设应避开正在发生抢占的后端。
func TestPreemptionSafePresetAvoidsPreempting(t *testing.T) {
	calm, thrash := newBackend(t, "calm"), newBackend(t, "thrash")
	calm.SetSnapshot(&backend.Snapshot{
		Time: time.Now(), Running: 5, KVUsage: 0.6, Raw: map[string]float64{},
	})
	// thrash 负载表面更低，但正在抢占（0.5 次/秒）——应被强惩罚。
	thrash.SetSnapshot(&backend.Snapshot{
		Time: time.Now(), Running: 2, KVUsage: 0.6,
		Raw: map[string]float64{"rate:vllm:num_preemptions_total": 0.5},
	})

	e := policy.NewEngine()
	pr := policy.FindPreset("preemption_safe")
	if err := e.Set("p", pr.Filter, pr.Score); err != nil {
		t.Fatalf("预设编译失败: %v", err)
	}
	s := New(e, nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "p"}
	route.SetPool([]*backend.Backend{calm, thrash})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "calm" {
		t.Fatalf("显存保守方案应避开抢占中的 thrash，实际选了 %s", got.ID)
	}
}

// TestPreemptRateVariable preempt_rate 的引擎归一化：vllm/sglang 计数取速率，缺失为 0。
func TestPreemptRateVariable(t *testing.T) {
	b := newBackend(t, "b1")
	env := New(policy.NewEngine(), nil, kvCfg()).BuildEnv(&Request{Model: "m"}, b, 0)
	if env["preempt_rate"] != 0.0 {
		t.Fatalf("无指标时 preempt_rate 应为 0，实际 %v", env["preempt_rate"])
	}
	b.SetSnapshot(&backend.Snapshot{
		Time: time.Now(),
		Raw:  map[string]float64{"rate:sglang:num_preemptions_total": 2.5},
	})
	env = New(policy.NewEngine(), nil, kvCfg()).BuildEnv(&Request{Model: "m"}, b, 0)
	if env["preempt_rate"] != 2.5 {
		t.Fatalf("sglang 抢占速率应取到 2.5，实际 %v", env["preempt_rate"])
	}
}

// TestTTFTEWMAVariable ttft_ewma 变量来自网关实测滑动均值。
func TestTTFTEWMAVariable(t *testing.T) {
	b := newBackend(t, "b1")
	b.ObserveTTFT(0.2)
	b.ObserveTTFT(0.4) // EWMA = 0.2*0.4 + 0.8*0.2 = 0.24
	env := New(policy.NewEngine(), nil, kvCfg()).BuildEnv(&Request{Model: "m"}, b, 0)
	got, _ := env["ttft_ewma"].(float64)
	if got < 0.23 || got > 0.25 {
		t.Fatalf("ttft_ewma 期望约 0.24，实际 %v", got)
	}
}
