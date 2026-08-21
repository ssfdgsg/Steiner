// PD 管理器单测：全互联建链、链路约束、decode 侧负载与链路拥塞打分。
package pd

import (
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/scheduler"
)

// newFixture 构建 registry + scheduler + pd manager。
func newFixture(t *testing.T, cfg *config.Config) (*Manager, *scheduler.Scheduler, *backend.Registry) {
	t.Helper()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("配置非法: %v", err)
	}
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("注册表构建失败: %v", err)
	}
	pol := policy.NewEngine()
	for name, pc := range cfg.Policies {
		if err := pol.Set(name, pc.Filter, pc.Score); err != nil {
			t.Fatalf("策略编译失败: %v", err)
		}
	}
	var tree *kvcache.Tree
	sched := scheduler.New(pol, tree, cfg.KVCache)
	m, err := NewManager(cfg, reg)
	if err != nil {
		t.Fatalf("PD 管理器构建失败: %v", err)
	}
	return m, sched, reg
}

func pdConfig(links []config.NCCLLinkConfig) *config.Config {
	return &config.Config{
		Backends: []config.BackendConfig{
			{ID: "p1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "p2", URL: "http://127.0.0.1:2", Engine: "vllm"},
			{ID: "d1", URL: "http://127.0.0.1:3", Engine: "vllm"},
			{ID: "d2", URL: "http://127.0.0.1:4", Engine: "vllm"},
		},
		PDGroups: []config.PDGroupConfig{{
			Name: "g", Prefill: []string{"p1", "p2"}, Decode: []string{"d1", "d2"}, Links: links,
		}},
		Models: []config.ModelRoute{{Name: "m", PDGroup: "g"}},
	}
}

func TestFullMeshWhenNoLinks(t *testing.T) {
	m, _, _ := newFixture(t, pdConfig(nil))
	g := m.Get("g")
	if g == nil {
		t.Fatal("组应存在")
	}
	links := g.Links()
	if len(links) != 4 { // 2 prefill × 2 decode
		t.Fatalf("全互联应有 4 条链路，实际 %d", len(links))
	}
}

func TestPickRespectsLinks(t *testing.T) {
	m, sched, _ := newFixture(t, pdConfig([]config.NCCLLinkConfig{
		{Prefill: "p1", Decode: "d2", BandwidthGbps: 400},
		{Prefill: "p2", Decode: "d1", BandwidthGbps: 400},
	}))
	g := m.Get("g")
	req := &scheduler.Request{Model: "m"}
	for i := 0; i < 10; i++ {
		prefill, decode, link, err := g.Pick(sched, req, nil, nil)
		if err != nil {
			t.Fatalf("Pick 失败: %v", err)
		}
		valid := (prefill.ID == "p1" && decode.ID == "d2") || (prefill.ID == "p2" && decode.ID == "d1")
		if !valid {
			t.Fatalf("配对 %s->%s 未走已建链组合", prefill.ID, decode.ID)
		}
		if link == nil || link.Prefill != prefill.ID || link.Decode != decode.ID {
			t.Fatalf("返回的链路与配对不符: %+v", link)
		}
	}
}

func TestPickSkipsUnhealthyDecode(t *testing.T) {
	m, sched, reg := newFixture(t, pdConfig(nil))
	reg.Get("d1").SetHealthy(false)
	g := m.Get("g")
	for i := 0; i < 5; i++ {
		_, decode, _, err := g.Pick(sched, &scheduler.Request{Model: "m"}, nil, nil)
		if err != nil {
			t.Fatalf("Pick 失败: %v", err)
		}
		if decode.ID == "d1" {
			t.Fatal("不健康的 decode 不应被配对")
		}
	}
}

func TestPickPrefersIdleLink(t *testing.T) {
	m, sched, reg := newFixture(t, pdConfig([]config.NCCLLinkConfig{
		{Prefill: "p1", Decode: "d1", BandwidthGbps: 400},
		{Prefill: "p1", Decode: "d2", BandwidthGbps: 400},
	}))
	// 排除 p2（无链路即无 decode 可配，Pick 内部会失败重试逻辑由 proxy 层处理，
	// 这里直接限定 prefill 只能是 p1）。
	exclude := map[string]bool{"p2": true}
	g := m.Get("g")

	// d1 负载高、d1 链路拥塞：应稳定配到 d2。
	reg.Get("d1").SetSnapshot(&backend.Snapshot{Time: time.Now(), Running: 50})
	for _, l := range g.Links() {
		_ = l
	}
	for i := 0; i < 5; i++ {
		_, decode, _, err := g.Pick(sched, &scheduler.Request{Model: "m"}, exclude, nil)
		if err != nil {
			t.Fatalf("Pick 失败: %v", err)
		}
		if decode.ID != "d2" {
			t.Fatalf("应优先空闲的 d2，实际 %s", decode.ID)
		}
	}
}

func TestNoDecodeLinkFails(t *testing.T) {
	m, sched, reg := newFixture(t, pdConfig([]config.NCCLLinkConfig{
		{Prefill: "p1", Decode: "d1", BandwidthGbps: 400},
		{Prefill: "p2", Decode: "d1", BandwidthGbps: 400},
	}))
	reg.Get("d1").SetHealthy(false) // 唯一有链路的 decode 下线
	g := m.Get("g")
	if _, _, _, err := g.Pick(sched, &scheduler.Request{Model: "m"}, nil, nil); err == nil {
		t.Fatal("无可用 decode 链路应报错")
	}
}

func TestLinkInflightAccounting(t *testing.T) {
	m, _, _ := newFixture(t, pdConfig(nil))
	g := m.Get("g")
	links := g.Links()
	if links[0].Inflight != 0 {
		t.Fatal("初始在途应为 0")
	}
	// 通过 linksByPrefill 找到具体链路做计数验证。
	var l *Link
	for _, ls := range g.linksByPrefill {
		l = ls[0]
		break
	}
	l.Acquire()
	l.Acquire()
	if l.Inflight() != 2 {
		t.Fatalf("在途计数期望 2，实际 %d", l.Inflight())
	}
	l.Release()
	if l.Inflight() != 1 {
		t.Fatalf("在途计数期望 1，实际 %d", l.Inflight())
	}
}

// ——— H9：会话粘性 PickPreferred ———

// TestPickPreferredHonorsBinding 绑定 prefill 可用时优先选中（KV 亲和）。
func TestPickPreferredHonorsBinding(t *testing.T) {
	m, sched, _ := newFixture(t, pdConfig(nil))
	g := m.Get("g")
	req := &scheduler.Request{Model: "m"}
	for i := 0; i < 10; i++ {
		p, _, _, err := g.PickPreferred(sched, req, "p2", nil, nil)
		if err != nil {
			t.Fatalf("PickPreferred 失败: %v", err)
		}
		if p.ID != "p2" {
			t.Fatalf("应优先绑定 prefill p2，实际 %s", p.ID)
		}
	}
}

// TestPickPreferredFallsBackWhenBoundUnavailable 绑定 prefill 已摘除/不可用时
// 回退普通选路（仍能成功选对）。
func TestPickPreferredFallsBackWhenBoundUnavailable(t *testing.T) {
	m, sched, reg := newFixture(t, pdConfig(nil))
	g := m.Get("g")
	// 熔断摘除 p1（健康摘除 → Available=false；摘除后组内切片陈旧是另一
	// 潜在问题，不在 H9 范围）。
	reg.Get("p1").MarkFailure(1, time.Minute)
	req := &scheduler.Request{Model: "m"}
	p, _, _, err := g.PickPreferred(sched, req, "p1", nil, nil)
	if err != nil {
		t.Fatalf("绑定失效应回退普通选路: %v", err)
	}
	if p.ID != "p2" {
		t.Fatalf("回退后应选 p2，实际 %s", p.ID)
	}
	// PrefillAvailable 同步返回 false（servePD 据此自愈 Unbind）。
	if g.PrefillAvailable("p1") {
		t.Fatal("熔断摘除的 prefill 不应可用")
	}
	if !g.PrefillAvailable("p2") {
		t.Fatal("存活 prefill 应可用")
	}
}

// TestPickPreferredRespectsExclude 绑定 prefill 被本轮排除（已失败）时回退。
func TestPickPreferredRespectsExclude(t *testing.T) {
	m, sched, _ := newFixture(t, pdConfig(nil))
	g := m.Get("g")
	req := &scheduler.Request{Model: "m"}
	p, _, _, err := g.PickPreferred(sched, req, "p2", map[string]bool{"p2": true}, nil)
	if err != nil {
		t.Fatalf("PickPreferred 失败: %v", err)
	}
	if p.ID != "p1" {
		t.Fatalf("排除 p2 后应回退 p1，实际 %s", p.ID)
	}
}
