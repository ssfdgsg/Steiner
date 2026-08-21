// 调度器单测：覆盖表达式打分、缓存亲和、失衡回退、轮询、一致性哈希、
// 最小负载、排除集合与打分解释。
package scheduler

import (
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/policy"
)

// newBackend 构造测试后端。
func newBackend(t *testing.T, id string) *backend.Backend {
	t.Helper()
	b, err := backend.New(config.BackendConfig{
		ID: id, URL: "http://127.0.0.1:1", Engine: "vllm", Weight: 1,
	})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	return b
}

// setLoad 设置后端快照中的负载。
func setLoad(b *backend.Backend, running, waiting, kvUsage float64) {
	b.SetSnapshot(&backend.Snapshot{
		Time: time.Now(), Running: running, Waiting: waiting, KVUsage: kvUsage,
		Raw: map[string]float64{},
	})
}

func kvCfg() config.KVCacheConfig {
	return config.KVCacheConfig{
		Enabled: true, MaxPrefixBytes: 4096,
		MatchThreshold: 0.3, BalanceAbsThreshold: 32, BalanceRelThreshold: 1.5,
	}
}

func newEngine(t *testing.T) *policy.Engine {
	t.Helper()
	e := policy.NewEngine()
	if err := e.Set("test", "healthy && kv_usage < 0.98", "running * 1.0 + waiting * 1.0"); err != nil {
		t.Fatalf("编译策略失败: %v", err)
	}
	return e
}

func TestExpressionPicksMinScore(t *testing.T) {
	b1, b2, b3 := newBackend(t, "b1"), newBackend(t, "b2"), newBackend(t, "b3")
	setLoad(b1, 10, 0, 0.5)
	setLoad(b2, 2, 0, 0.5)
	setLoad(b3, 7, 0, 0.5)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "test"}
	route.SetPool([]*backend.Backend{b1, b2, b3})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("应选最低分 b2，实际 %s", got.ID)
	}
}

func TestExpressionFilterExcludes(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 0, 0, 0.99) // kv_usage 超过 filter 上限
	setLoad(b2, 50, 0, 0.5)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "test"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("b1 应被 filter 淘汰，实际选了 %s", got.ID)
	}
}

func TestExpressionAllFilteredFallsBack(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 5, 0, 0.99)
	setLoad(b2, 1, 0, 0.99)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "test"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("全部被过滤时应降级而非失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("降级应选最小负载 b2，实际 %s", got.ID)
	}
}

func TestExcludeSet(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 0, 0, 0)
	setLoad(b2, 10, 0, 0)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "test"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m"}, map[string]bool{"b1": true})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("b1 被排除后应选 b2，实际 %s", got.ID)
	}
}

func TestUnhealthyExcluded(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 0, 0, 0)
	setLoad(b2, 10, 0, 0)
	b1.SetHealthy(false)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "least_request"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("不健康的 b1 不应参与调度，实际 %s", got.ID)
	}
}

func TestNoCandidates(t *testing.T) {
	b1 := newBackend(t, "b1")
	b1.SetHealthy(false)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "least_request"}
	route.SetPool([]*backend.Backend{b1})

	if _, err := s.Pick(route, &Request{Model: "m"}, nil); err == nil {
		t.Fatal("无可用后端应返回错误")
	}
}

func TestRoundRobinCycles(t *testing.T) {
	b1, b2, b3 := newBackend(t, "b1"), newBackend(t, "b2"), newBackend(t, "b3")
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "round_robin"}
	route.SetPool([]*backend.Backend{b1, b2, b3})

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		got, err := s.Pick(route, &Request{Model: "m"}, nil)
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		seen[got.ID]++
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		if seen[id] != 3 {
			t.Fatalf("轮询应均匀分布，实际 %v", seen)
		}
	}
}

func TestLeastRequest(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 5, 5, 0)
	setLoad(b2, 1, 0, 0)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "least_request"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("最小负载应选 b2，实际 %s", got.ID)
	}
}

func TestConsistentHashStable(t *testing.T) {
	b1, b2, b3 := newBackend(t, "b1"), newBackend(t, "b2"), newBackend(t, "b3")
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "consistent_hash"}
	route.SetPool([]*backend.Backend{b1, b2, b3})

	req := &Request{Model: "m", SessionID: "user-42"}
	first, err := s.Pick(route, req, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := s.Pick(route, req, nil)
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if got.ID != first.ID {
			t.Fatalf("同一会话应稳定路由到 %s，实际 %s", first.ID, got.ID)
		}
	}
	// 目标后端不可用时应转移到其他后端而非失败。
	first.SetHealthy(false)
	got, err := s.Pick(route, req, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID == first.ID {
		t.Fatal("不可用后端不应再被命中")
	}
}

func TestCacheAwareAffinity(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 0, 0, 0)
	setLoad(b2, 3, 0, 0)
	tree := kvcache.NewTree(4096, time.Minute)
	prompt := "system:固定系统提示词ABCDEFG\nuser:今天天气如何"
	tree.Insert(prompt, "b2")

	s := New(newEngine(t), tree, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "cache_aware"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m", PromptText: prompt}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("前缀命中应亲和到 b2，实际 %s", got.ID)
	}
}

func TestCacheAwareImbalanceFallback(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 0, 0, 0)
	setLoad(b2, 100, 50, 0) // b2 严重过载：超过 abs=32 且远超 1.5*minLoad
	tree := kvcache.NewTree(4096, time.Minute)
	prompt := "system:固定系统提示词ABCDEFG\nuser:今天天气如何"
	tree.Insert(prompt, "b2")

	s := New(newEngine(t), tree, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "cache_aware"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m", PromptText: prompt}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b1" {
		t.Fatalf("目标过载应回退最小负载 b1，实际 %s", got.ID)
	}
}

func TestObserveFeedsPrefixMatch(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 0, 0, 0)
	setLoad(b2, 0, 0, 0)
	tree := kvcache.NewTree(4096, time.Minute)
	e := policy.NewEngine()
	// 分数只由前缀命中决定：命中越多分越低。
	if err := e.Set("test", "healthy", "10.0 - prefix_match * 10.0"); err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	s := New(e, tree, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "test"}
	route.SetPool([]*backend.Backend{b1, b2})

	req := &Request{Model: "m", PromptText: "多轮对话的公共前缀内容", PromptLen: 30}
	s.Observe(req, "b1")

	got, err := s.Pick(route, req, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b1" {
		t.Fatalf("Observe 后 prefix_match 应引导到 b1，实际 %s", got.ID)
	}
}

func TestExplain(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	setLoad(b1, 8, 0, 0)
	setLoad(b2, 2, 0, 0)
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "test"}
	route.SetPool([]*backend.Backend{b1, b2})

	details := s.Explain(route, "", &Request{Model: "m"})
	if len(details) != 2 {
		t.Fatalf("应有 2 条明细，实际 %d", len(details))
	}
	if details[0].Backend != "b2" || details[0].Score >= details[1].Score {
		t.Fatalf("明细应按分数升序: %+v", details)
	}
}

func TestConcurrencyCapExcludes(t *testing.T) {
	b1, err := backend.New(config.BackendConfig{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm", Weight: 1, MaxConcurrency: 1})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	b2 := newBackend(t, "b2")
	setLoad(b1, 0, 0, 0)
	setLoad(b2, 10, 0, 0)
	if !b1.TryAcquire() {
		t.Fatal("首次占用应成功")
	}
	s := New(newEngine(t), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "least_request"}
	route.SetPool([]*backend.Backend{b1, b2})

	got, err := s.Pick(route, &Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got.ID != "b2" {
		t.Fatalf("并发满额的 b1 不应入选，实际 %s", got.ID)
	}
}

func TestPromptRequirements(t *testing.T) {
	e := policy.NewEngine()
	if err := e.Set("default", "healthy", "running - prefix_match"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set("cost-aware", "healthy", "running + prompt_len + image_count"); err != nil {
		t.Fatal(err)
	}

	route := &backend.Route{Name: "m", Strategy: "expression", PolicyName: "default"}
	s := New(e, nil, kvCfg())
	if features, limit := s.PromptRequirements(route, false); features || limit != 0 {
		t.Fatalf("无 KV 且策略只用 prefix_match 时应走轻量解析: features=%v limit=%d", features, limit)
	}

	route.PolicyName = "cost-aware"
	if features, limit := s.PromptRequirements(route, false); !features || limit != 0 {
		t.Fatalf("策略使用 prompt 特征时应解析但不保留文本: features=%v limit=%d", features, limit)
	}

	route.Strategy, route.PolicyName = "consistent_hash", ""
	if features, limit := s.PromptRequirements(route, false); !features || limit != -1 {
		t.Fatalf("无 session 的一致性哈希应保留完整 prompt: features=%v limit=%d", features, limit)
	}
	if features, limit := s.PromptRequirements(route, true); features || limit != 0 {
		t.Fatalf("有 session 的一致性哈希不需要 prompt: features=%v limit=%d", features, limit)
	}

	route.Strategy = "least_request"
	s = New(e, kvcache.NewTree(4096, time.Minute), kvCfg())
	if features, limit := s.PromptRequirements(route, false); !features || limit != 4096 {
		t.Fatalf("KV 前缀树应只保留配置上限: features=%v limit=%d", features, limit)
	}
}
