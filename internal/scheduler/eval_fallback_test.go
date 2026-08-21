// H7 复现性单测（修复后语义）：策略 filter 表达式运行期求值错误（未知变量等）时，
// 调度器静默跳过该后端；若全部候选都求值失败，**不得**降级 pickLeast——
// 否则 filter 约束被完全绕过，请求仍被发往本应被过滤的后端。
//
// 断言的是**修复后行为**：filter 引用不存在的变量（如拼写错误 waitting）时
// 选路必须显式失败（fail-closed）；只有"合法过滤干净"（pass=false 且零错误）
// 才允许 pickLeast 兜底。
package scheduler

import (
	"testing"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
)

// TestH7FilterEvalErrorSilentlyBypassed 复现 H7（修复后翻转）：
// filter = "waitting < 32"（变量拼错，运行时求值必错）时，
// 全部候选求值失败 → Pick 必须返回错误，不得静默降级返回后端。
func TestH7FilterEvalErrorSilentlyBypassed(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "vllm"},
		},
		Models: []config.ModelRoute{
			{Name: "m1", Backends: []string{"b1", "b2"}, Strategy: "expression", Policy: "p_err"},
		},
		// 策略在配置中声明(编译期放行未知变量)，运行期求值才报错。
		Policies: map[string]config.PolicyConfig{
			"p_err": {Filter: "waitting < 32", Score: "inflight"},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("配置非法: %v", err)
	}
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("注册表构建失败: %v", err)
	}
	pol := policy.NewEngine()
	// waitting 是拼错的变量名：编译期 AllowUndefinedVariables 放行,
	// 运行期 env 无此键 → Eval 返回错误 → 每个候选都被跳过。
	if err := pol.Set("p_err", "waitting < 32", "inflight"); err != nil {
		t.Fatalf("策略编译失败（现状应成功）: %v", err)
	}
	s := New(pol, nil, config.KVCacheConfig{})
	rt, _ := reg.Route("m1")
	req := &Request{Model: "m1"}

	b, err := s.Pick(rt, req, nil)
	if err == nil {
		// 修复前：静默降级返回后端，filter 约束（waitting<32）被完全绕过。
		t.Fatalf("H7 未修复：filter 求值全部出错仍降级返回后端 %v，应显式报错拒绝绕过 filter", b)
	}
	// 修复后：求值错误 ≠ 合法过滤，fail-closed 显式失败。
}

// TestH7CorrectFilterRejectsAll 对照组：同样的表达式策略，但 filter 用合法变量
// 且条件不可满足（waiting < 0 恒假）时，全部被**合法过滤**（pass=false 且零求值
// 错误）→ pickLeast 仍兜底返回。该行为是设计中的降级路径，与 H7 的区别：
// 这里是"合法表达式、确实无候选"，不受 fail-closed 影响，断言保持不变。
func TestH7CorrectFilterRejectsAll(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
		},
		Models: []config.ModelRoute{
			{Name: "m1", Backends: []string{"b1"}, Strategy: "expression", Policy: "p_strict"},
		},
	}
	cfg.ApplyDefaults()
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("注册表构建失败: %v", err)
	}
	pol := policy.NewEngine()
	if err := pol.Set("p_strict", "waiting < 0", "inflight"); err != nil {
		t.Fatalf("策略编译失败: %v", err)
	}
	s := New(pol, nil, config.KVCacheConfig{})
	rt, _ := reg.Route("m1")
	b, err := s.Pick(rt, &Request{Model: "m1"}, nil)
	if err != nil || b == nil {
		t.Fatalf("过滤后降级路径应仍返回后端，实际 err=%v b=%v", err, b)
	}
}
