// 注册表动态增删单测：池 copy-on-write、幂等 Upsert、约束校验与子池收缩。
package backend

import (
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
)

func dynCfg() *config.Config {
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

func TestAddRemoveBackend(t *testing.T) {
	reg, err := NewRegistry(dynCfg())
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	rt, _ := reg.Route("m1")
	oldPool := rt.Pool()

	bc := config.BackendConfig{ID: "b3", URL: "http://127.0.0.1:3", Engine: "vllm"}
	bc.ApplyDefaults()
	if _, err := reg.AddBackend(bc, []string{"m1"}); err != nil {
		t.Fatalf("动态注册失败: %v", err)
	}
	if len(rt.Pool()) != 3 || reg.Get("b3") == nil || len(reg.All()) != 3 {
		t.Fatalf("注册后状态不符: pool=%d all=%d", len(rt.Pool()), len(reg.All()))
	}
	// copy-on-write：注册前取得的池快照不受影响。
	if len(oldPool) != 2 {
		t.Fatalf("旧池快照被修改: %d", len(oldPool))
	}

	if err := reg.RemoveBackend("b1"); err != nil {
		t.Fatalf("摘除失败: %v", err)
	}
	if len(rt.Pool()) != 2 || reg.Get("b1") != nil {
		t.Fatalf("摘除后全池状态不符: %d", len(rt.Pool()))
	}
	// 子池同步收缩：AddBackend(b3) 后按 H10 契约 s1=[b1,b3]、s2=[b2,b3]；
	// 摘除 b1 后 s1 应只剩 b3（从子池移除，与新后端入池保持对称）。
	if pool := rt.Splits[0].Pool(); len(pool) != 1 || pool[0].ID != "b3" {
		t.Fatalf("子池 s1 应只剩 b3，实际 %d 个", len(pool))
	}
}

func TestAddBackendConstraints(t *testing.T) {
	cfg := dynCfg()
	cfg.Backends = append(cfg.Backends, config.BackendConfig{ID: "p1", URL: "http://127.0.0.1:4", Engine: "vllm"},
		config.BackendConfig{ID: "d1", URL: "http://127.0.0.1:5", Engine: "vllm"})
	cfg.PDGroups = []config.PDGroupConfig{{Name: "g1", Prefill: []string{"p1"}, Decode: []string{"d1"}}}
	cfg.Models = append(cfg.Models, config.ModelRoute{Name: "m-pd", PDGroup: "g1"})
	cfg.ApplyDefaults()
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}

	bc := config.BackendConfig{ID: "b9", URL: "http://127.0.0.1:9", Engine: "vllm"}
	bc.ApplyDefaults()
	if _, err := reg.AddBackend(bc, nil); err == nil {
		t.Fatal("未指定模型路由应报错")
	}
	if _, err := reg.AddBackend(bc, []string{"不存在"}); err == nil {
		t.Fatal("路由不存在应报错")
	}
	if _, err := reg.AddBackend(bc, []string{"m-pd"}); err == nil {
		t.Fatal("加入 PD 路由应报错")
	}
	dup := config.BackendConfig{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"}
	dup.ApplyDefaults()
	if _, err := reg.AddBackend(dup, []string{"m1"}); err == nil {
		t.Fatal("重复 ID 应报错")
	}
}

func TestUpsertBackend(t *testing.T) {
	reg, err := NewRegistry(dynCfg())
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	// 覆盖已有 b1：URL 变更后实例应被替换而非重复。
	bc := config.BackendConfig{ID: "b1", URL: "http://127.0.0.1:100", Engine: "vllm"}
	bc.ApplyDefaults()
	if _, err := reg.UpsertBackend(bc, []string{"m1"}); err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	if got := reg.Get("b1").URL.Port(); got != "100" {
		t.Fatalf("upsert 未替换实例: port=%s", got)
	}
	if n := len(reg.All()); n != 2 {
		t.Fatalf("upsert 后总数应为 2，实际 %d", n)
	}
}

// TestRemoveBackendInvokesRemovedHook L4 回归(缺陷翻转)：修复前后端摘除
// 只改路由池，旁路状态（kvcache 前缀树归属）无联动清理入口；修复后摘除
// 链路上回调装配层注入的 hook（RemoveBackend 与 Upsert 替换旧实例均触发）。
func TestRemoveBackendInvokesRemovedHook(t *testing.T) {
	reg, err := NewRegistry(dynCfg())
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	var removed []string
	reg.SetBackendRemovedHook(func(id string) { removed = append(removed, id) })

	if err := reg.RemoveBackend("b1"); err != nil {
		t.Fatalf("摘除失败: %v", err)
	}
	if len(removed) != 1 || removed[0] != "b1" {
		t.Fatalf("摘除回调应收到 b1，实际 %v", removed)
	}
	// 不存在的 ID：报错且不触发回调。
	if err := reg.RemoveBackend("不存在"); err == nil {
		t.Fatal("摘除不存在的后端应报错")
	}
	if len(removed) != 1 {
		t.Fatalf("失败路径不应触发回调: %v", removed)
	}
	// Upsert 替换同 ID 旧实例同样触发：新实例不"继承"旧归属（L4 危害点）。
	// b1 已摘除，用仍存在的 b2 验证替换路径。
	bc := config.BackendConfig{ID: "b2", URL: "http://127.0.0.1:200", Engine: "vllm"}
	bc.ApplyDefaults()
	if _, err := reg.UpsertBackend(bc, []string{"m1"}); err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	if len(removed) != 2 || removed[1] != "b2" {
		t.Fatalf("替换旧实例应触发回调: %v", removed)
	}
}

// TestRemoveBackendCleansTreeOwners L4 端到端（registry × kvcache 树）：
// 摘除后端后，前缀树内该后端的归属立即清空（不等 TTL），其他后端不受影响。
func TestRemoveBackendCleansTreeOwners(t *testing.T) {
	reg, err := NewRegistry(dynCfg())
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	// 装配层接线：树创建后注入摘除回调（等价 cmd/gateway/main.go 的接线）。
	tr := kvcache.NewTree(4096, time.Hour) // TTL 1 小时：残留窗口极长，更暴露 L4
	reg.SetBackendRemovedHook(func(id string) { tr.RemoveBackedBy(id) })

	tr.Insert("system:公共提示词\nuser:你好", "b1")
	tr.Insert("system:公共提示词\nuser:你好", "b2")
	if best, _ := tr.Match("system:公共提示词\nuser:你好"); best["b1"] == 0 {
		t.Fatalf("前置条件失败：b1 应有归属: %v", best)
	}

	if err := reg.RemoveBackend("b1"); err != nil {
		t.Fatalf("摘除失败: %v", err)
	}
	best, _ := tr.Match("system:公共提示词\nuser:你好")
	if _, ok := best["b1"]; ok {
		t.Fatalf("L4 修复失败：摘除后树内仍残留 b1 归属（应联动清理）: %v", best)
	}
	if best["b2"] == 0 {
		t.Fatalf("b2 的归属不应被误删: %v", best)
	}
}
