// 预设方案库单测：全部预设可编译可求值、查找与反查、名称清单。
package policy

import "testing"

// TestAllPresetsCompileAndEval 每个预设都必须能编译并在标准环境下求值——
// 预设只允许使用 Go 侧预计算的保证存在的变量。
func TestAllPresetsCompileAndEval(t *testing.T) {
	e := NewEngine()
	env := baseEnv(map[string]interface{}{
		"running": 3.0, "waiting": 2.0, "kv_usage": 0.5, "inflight": 1.0,
		"prefix_match": 0.4, "ttft_ewma": 0.12, "preempt_rate": 0.5, "gen_tps": 800.0,
	})
	for _, pr := range Presets {
		if err := e.Set(pr.Name, pr.Filter, pr.Score); err != nil {
			t.Fatalf("预设 %s 编译失败: %v", pr.Name, err)
		}
		pass, score, err := e.Get(pr.Name).Eval(env)
		if err != nil {
			t.Fatalf("预设 %s 求值失败: %v", pr.Name, err)
		}
		if !pass {
			t.Fatalf("预设 %s 在健康低压环境下不应过滤候选", pr.Name)
		}
		_ = score
	}
}

// TestPresetFiltersRejectHighKV 全部预设的 filter 都应拒绝 KV 逼近满载的后端
// （水位阈值各不相同，但 0.99 必须全部拒绝）。
func TestPresetFiltersRejectHighKV(t *testing.T) {
	e := NewEngine()
	env := baseEnv(map[string]interface{}{"kv_usage": 0.99})
	for _, pr := range Presets {
		if err := e.Set(pr.Name, pr.Filter, pr.Score); err != nil {
			t.Fatalf("预设 %s 编译失败: %v", pr.Name, err)
		}
		pass, _, err := e.Get(pr.Name).Eval(env)
		if err != nil {
			t.Fatalf("预设 %s 求值失败: %v", pr.Name, err)
		}
		if pass {
			t.Fatalf("预设 %s 不应放行 kv_usage=0.99 的后端", pr.Name)
		}
	}
}

func TestFindPreset(t *testing.T) {
	if FindPreset("balanced") == nil {
		t.Fatal("balanced 预设应存在")
	}
	if FindPreset("不存在的方案") != nil {
		t.Fatal("未知预设应返回 nil")
	}
}

func TestMatchPreset(t *testing.T) {
	for _, pr := range Presets {
		if got := MatchPreset(pr.Filter, pr.Score); got != pr.Name {
			t.Fatalf("预设 %s 反查失败，得到 %q", pr.Name, got)
		}
	}
	if got := MatchPreset("healthy", "running * 1.23"); got != "custom" {
		t.Fatalf("手写表达式应反查为 custom，得到 %q", got)
	}
}

func TestPresetNames(t *testing.T) {
	names := PresetNames()
	if len(names) != len(Presets) || names[0] != "balanced" {
		t.Fatalf("名称清单不符: %v", names)
	}
}

// TestPresetMetadata 前端展示字段必须齐全。
func TestPresetMetadata(t *testing.T) {
	seen := map[string]bool{}
	for _, pr := range Presets {
		if pr.Name == "" || pr.Title == "" || pr.Description == "" {
			t.Fatalf("预设 %+v 缺少展示字段", pr)
		}
		if seen[pr.Name] {
			t.Fatalf("预设名重复: %s", pr.Name)
		}
		seen[pr.Name] = true
	}
}
