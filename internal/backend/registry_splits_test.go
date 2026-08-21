// H10 修复后单测：动态注册的后端与全池保持一致，同步进入路由的每一个
// splits 子池（去重），分池（金丝雀/灰度）路由正常流量因此也能选中它。
//
// 断言的是**修复后行为**：注册后全池包含新后端、每个子池恰好包含一次；
// 调度侧 least_request 同负载不抢占旧后端属于预期行为，不在本测试范围。
package backend

import (
	"testing"

	"ai-gateway/internal/config"
)

// TestH10DynamicBackendNotInSplits 验证 H10 修复的注册表层行为：
// AddBackend 成功后新后端同步进入每个 splits 子池（去重，无重复）。
func TestH10DynamicBackendNotInSplits(t *testing.T) {
	reg, err := NewRegistry(dynCfg())
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	rt, _ := reg.Route("m1")

	bc := config.BackendConfig{ID: "b3", URL: "http://127.0.0.1:3", Engine: "vllm"}
	bc.ApplyDefaults()
	if _, err := reg.AddBackend(bc, []string{"m1"}); err != nil {
		t.Fatalf("动态注册失败（现状应成功）: %v", err)
	}

	// 全池包含 b3（注册生效）
	if n := len(rt.Pool()); n != 3 {
		t.Fatalf("全池应为 3，实际 %d", n)
	}
	// 每个子池都必须包含 b3，且恰好一次（去重，无重复）。
	for i, sp := range rt.Splits {
		count := 0
		for _, b := range sp.Pool() {
			if b.ID == "b3" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("子池 s[%d]（%s）应恰好包含 1 个 b3，实际 %d", i, sp.Name, count)
		}
	}
}
