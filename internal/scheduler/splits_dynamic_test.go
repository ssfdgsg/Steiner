// H10 复现性单测（调度层）：动态注册的后端只进路由"全池"、不进 splits 子池，
// 分池（金丝雀/灰度）路由的子池健康时，调度器永远选不中它（僵尸后端，零流量）。
//
// 断言的是**当前缺陷行为**；修复（动态后端同步进子池）后需翻转断言。
package scheduler

import (
	"testing"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
)

func splitsCfg() *config.Config {
	c := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "vllm"},
		},
		Models: []config.ModelRoute{
			{Name: "m1", Backends: []string{"b1", "b2"}, Splits: []config.SplitConfig{
				{Name: "s1", Weight: 1, Backends: []string{"b1"}},
				{Name: "s2", Weight: 1, Backends: []string{"b2"}},
			}},
		},
	}
	c.ApplyDefaults()
	return c
}

// TestH10SchedulerNeverPicksDynamicBackend 复现 H10 的调度层后果：
// 子池健康时，100 次选路永不命中动态注册的后端 b3。
func TestH10SchedulerNeverPicksDynamicBackend(t *testing.T) {
	reg, err := backend.NewRegistry(splitsCfg())
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	bc := config.BackendConfig{ID: "b3", URL: "http://127.0.0.1:3", Engine: "vllm"}
	bc.ApplyDefaults()
	if _, err := reg.AddBackend(bc, []string{"m1"}); err != nil {
		t.Fatalf("动态注册失败（现状应成功）: %v", err)
	}

	pol := policy.NewEngine()
	if err := pol.Set("default", "healthy", "inflight"); err != nil {
		t.Fatalf("默认策略编译失败: %v", err)
	}
	s := New(pol, nil, config.KVCacheConfig{})
	rt, _ := reg.Route("m1")
	req := &Request{Model: "m1"}

	for i := 0; i < 100; i++ {
		b, err := s.Pick(rt, req, nil)
		if err != nil {
			t.Fatalf("第 %d 次选路失败: %v", i, err)
		}
		if b.ID == "b3" {
			t.Fatalf("H10 复现失败：调度器选中了动态后端 b3（缺陷不存在），请翻转断言")
		}
	}
}
