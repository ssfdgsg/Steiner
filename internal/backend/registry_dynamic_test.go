// 注册表动态增删单测：池 copy-on-write、幂等 Upsert、约束校验与子池收缩。
package backend

import (
	"testing"

	"ai-gateway/internal/config"
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
	// 子池同步收缩：s1 只有 b1，摘除后应为空。
	if n := len(rt.Splits[0].Pool()); n != 0 {
		t.Fatalf("子池 s1 应为空，实际 %d", n)
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
