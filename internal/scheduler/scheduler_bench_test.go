// 调度选路基准：16 后端池上各策略的单次 Pick 耗时。
// 作为性能基线记录在 docs/perf-baseline.md，改动调度器后运行
// `make bench` 对照防回退。
package scheduler

import (
	"fmt"
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/policy"
)

// benchPool 构造 n 个带负载快照的后端。
func benchPool(b *testing.B, n int) []*backend.Backend {
	b.Helper()
	pool := make([]*backend.Backend, 0, n)
	for i := 0; i < n; i++ {
		bk, err := backend.New(config.BackendConfig{
			ID: fmt.Sprintf("b%02d", i), URL: "http://127.0.0.1:1", Engine: "vllm", Weight: 1,
		})
		if err != nil {
			b.Fatalf("构造后端失败: %v", err)
		}
		bk.SetSnapshot(&backend.Snapshot{
			Time: time.Now(), Running: float64(i % 7), Waiting: float64(i % 3),
			KVUsage: 0.1 * float64(i%9), Raw: map[string]float64{},
		})
		pool = append(pool, bk)
	}
	return pool
}

func BenchmarkPick(b *testing.B) {
	pol := policy.NewEngine()
	if err := pol.Set("bench", "healthy && kv_usage < 0.98",
		"running * 2.0 + waiting * 6.0 + inflight * 1.0 + kv_usage * 8.0 - prefix_match * 10.0"); err != nil {
		b.Fatalf("编译策略失败: %v", err)
	}
	tree := kvcache.NewTree(4096, 10*time.Minute)
	pool := benchPool(b, 16)
	// 预热前缀树：模拟已有亲和记录。
	for i, bk := range pool {
		tree.Insert(fmt.Sprintf("系统提示词公共前缀，租户 %d 的历史对话内容", i%4), bk.ID)
	}
	s := New(pol, tree, config.KVCacheConfig{
		Enabled: true, MaxPrefixBytes: 4096,
		MatchThreshold: 0.3, BalanceAbsThreshold: 32, BalanceRelThreshold: 1.5,
	})

	req := &Request{
		Model:      "m1",
		PromptText: "系统提示词公共前缀，租户 1 的历史对话内容，以及本轮的新问题",
		PromptLen:  64,
		SessionID:  "sess-bench",
	}
	for _, strategy := range []string{
		"round_robin", "random", "weighted_random", "least_request",
		"p2c", "consistent_hash", "cache_aware", "expression",
	} {
		route := &backend.Route{Name: "m1", Strategy: strategy, PolicyName: "bench"}
		route.SetPool(pool)
		b.Run(strategy, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := s.Pick(route, req, nil); err != nil {
					b.Fatalf("选路失败: %v", err)
				}
			}
		})
	}
}
