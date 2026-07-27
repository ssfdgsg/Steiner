// 模型名改写与权重分池（金丝雀）的代理层测试。
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/test/mockbackend"
)

// TestRewriteModel 验证请求体 model 字段在转发前被改写（mock 会回显收到的 model）。
func TestRewriteModel(t *testing.T) {
	mock := mockbackend.New("vllm")
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: srv.URL, Engine: "vllm"}},
		Models: []config.ModelRoute{{
			Name: "gpt-4", Backends: []string{"b1"}, RewriteModel: "qwen3-32b",
		}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4", false)))
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("应答解析失败: %v", err)
	}
	if resp["model"] != "qwen3-32b" {
		t.Fatalf("后端收到的 model 应为改写后的 qwen3-32b，实际 %v", resp["model"])
	}
}

// TestSplitRouting 验证分池分流：全部权重在 canary 子池时流量只进 b2；
// canary 后端隔离后回退全池（b1 兜底），保证可用性优先于分流比例。
func TestSplitRouting(t *testing.T) {
	stable := httptest.NewServer(mockbackend.New("vllm").Handler())
	defer stable.Close()
	canary := httptest.NewServer(mockbackend.New("vllm").Handler())
	defer canary.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: stable.URL, Engine: "vllm"},
			{ID: "b2", URL: canary.URL, Engine: "vllm"},
		},
		Models: []config.ModelRoute{{
			Name: "m1",
			Splits: []config.SplitConfig{
				// 权重悬殊到 1e18 倍：canary 未被选中的概率可忽略，测试确定性成立。
				{Name: "stable", Backends: []string{"b1"}, Weight: 1e-9},
				{Name: "canary", Backends: []string{"b2"}, Weight: 1e9},
			},
		}},
	})

	send := func() string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		st.handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("X-Upstream-Backend")
	}

	// canary 权重占绝对多数 -> 全部命中 b2。
	for i := 0; i < 5; i++ {
		if got := send(); got != "b2" {
			t.Fatalf("第 %d 次应命中 canary 子池 b2，实际 %s", i, got)
		}
	}
	route, err := st.reg.Route("m1")
	if err != nil {
		t.Fatalf("查路由失败: %v", err)
	}
	if len(route.Pool()) != 2 {
		t.Fatalf("全池应为子池并集（2 个后端），实际 %d", len(route.Pool()))
	}
	if hits := route.Splits[1].Hits.Load(); hits != 5 {
		t.Fatalf("canary 命中计数应为 5，实际 %d", hits)
	}

	// 隔离 canary 后端 -> 子池无可用 -> 回退全池，b1 接流。
	st.reg.Get("b2").Cordon(true)
	if got := send(); got != "b1" {
		t.Fatalf("canary 隔离后应回退到 b1，实际 %s", got)
	}
}

// TestSplitConfigValidate 验证分池配置的校验规则。
func TestSplitConfigValidate(t *testing.T) {
	base := func(splits []config.SplitConfig) *config.Config {
		return &config.Config{
			Backends: []config.BackendConfig{{ID: "b1", URL: "http://x", Engine: "vllm"}},
			Models:   []config.ModelRoute{{Name: "m1", Splits: splits}},
		}
	}
	cases := []struct {
		名称     string
		splits []config.SplitConfig
		合法     bool
	}{
		{"正常", []config.SplitConfig{{Name: "a", Backends: []string{"b1"}, Weight: 1}}, true},
		{"缺少name", []config.SplitConfig{{Backends: []string{"b1"}}}, false},
		{"名称重复", []config.SplitConfig{
			{Name: "a", Backends: []string{"b1"}}, {Name: "a", Backends: []string{"b1"}},
		}, false},
		{"引用不存在后端", []config.SplitConfig{{Name: "a", Backends: []string{"没有"}}}, false},
		{"空backends", []config.SplitConfig{{Name: "a"}}, false},
	}
	for _, tc := range cases {
		c := base(tc.splits)
		c.ApplyDefaults()
		err := c.Validate()
		if tc.合法 && err != nil {
			t.Fatalf("用例 %s 不应报错: %v", tc.名称, err)
		}
		if !tc.合法 && err == nil {
			t.Fatalf("用例 %s 应报错", tc.名称)
		}
	}

	// splits 与 backends 互斥。
	c := base([]config.SplitConfig{{Name: "a", Backends: []string{"b1"}}})
	c.Models[0].Backends = []string{"b1"}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("backends 与 splits 同时配置应报错")
	}

	// 默认值继承：子池未写 strategy/policy 时继承路由。
	c = base([]config.SplitConfig{{Name: "a", Backends: []string{"b1"}}})
	c.ApplyDefaults()
	if sp := c.Models[0].Splits[0]; sp.Strategy != "expression" || sp.Policy != config.DefaultPolicyName || sp.Weight != 1 {
		t.Fatalf("子池默认值不符: %+v", sp)
	}
}
