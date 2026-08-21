// 加权随机调度单测：覆盖 pickWeighted 的分布与 M15（权重和≤0）防护。
package scheduler

import (
	"testing"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

func weightedRoute(t *testing.T, weights ...float64) *backend.Route {
	t.Helper()
	bs := make([]*backend.Backend, len(weights))
	for i, w := range weights {
		b, err := backend.New(config.BackendConfig{ID: string(rune('a' + i)), URL: "http://127.0.0.1:1", Engine: "vllm", Weight: w})
		if err != nil {
			t.Fatalf("构造后端失败: %v", err)
		}
		bs[i] = b
	}
	route := &backend.Route{Name: "m", Strategy: "weighted_random"}
	route.SetPool(bs)
	return route
}

// TestWeightedRandomDistribution 正权重分布：权重越大被选次数越多（粗粒度 20 次）。
func TestWeightedRandomDistribution(t *testing.T) {
	s := New(newEngine(t), nil, kvCfg())
	route := weightedRoute(t, 1, 3, 6)
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		b, err := s.Pick(route, &Request{Model: "m"}, nil)
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		counts[b.ID]++
	}
	if counts["c"] <= counts["a"] {
		t.Fatalf("权重 6 的候选应明显多于权重 1 的候选: %v", counts)
	}
	total := counts["a"] + counts["b"] + counts["c"]
	if total != 20 {
		t.Fatalf("应恰好返回 20 次: %v", counts)
	}
}

// TestWeightedRandomZeroSum M15 防护：即使绕过构造期钳制（直接改 Weight 字段
// 模拟旁路构造路径），权重和≤0 时仍确定性返回首候选，不 panic 不绕圈。
func TestWeightedRandomZeroSum(t *testing.T) {
	s := New(newEngine(t), nil, kvCfg())
	for _, weights := range [][]float64{{0, 0, 0}, {-1, -2}, {0, -3}} {
		route := weightedRoute(t, weights...)
		// 构造期 normalizeWeight 已钳制，这里直接覆写模拟旁路路径。
		for i, w := range weights {
			route.Pool()[i].Weight = w
		}
		got, err := s.Pick(route, &Request{Model: "m"}, nil)
		if err != nil {
			t.Fatalf("权重 %v 不应报错: %v", weights, err)
		}
		if got.ID != string(rune('a')) {
			t.Fatalf("权重和≤0(%v) 应确定性返回首候选，实际 %s", weights, got.ID)
		}
	}
}

// TestWeightedRandomSingle 单候选退化：权重任意都返回它。
func TestWeightedRandomSingle(t *testing.T) {
	s := New(newEngine(t), nil, kvCfg())
	route := weightedRoute(t, 0)
	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != string(rune('a')) {
		t.Fatalf("单候选应返回自身，实际 %s", got.ID)
	}
}
