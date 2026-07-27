// 接入层单测：路由注册、管理面端点（后端清单、策略热更、打分解释、隔离）。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/scheduler"
)

// newTestServer 装配最小可用的完整服务。
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "sglang"},
		},
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}}},
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
	for name, pc := range cfg.Policies {
		if err := pol.Set(name, pc.Filter, pc.Score); err != nil {
			t.Fatalf("策略编译失败: %v", err)
		}
	}
	tree := kvcache.NewTree(4096, 0)
	sched := scheduler.New(pol, tree, cfg.KVCache)
	pdMgr, err := pd.NewManager(cfg, reg)
	if err != nil {
		t.Fatalf("PD 管理器构建失败: %v", err)
	}
	gw := metrics.NewGateway()
	px := proxy.New(cfg, reg, sched, pdMgr, gw)
	return New(cfg, reg, pol, sched, tree, pdMgr, gw, px)
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/healthz", ""); rec.Code != 200 {
		t.Fatalf("healthz 期望 200，实际 %d", rec.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/metrics", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Fatalf("自身指标端点异常: code=%d", rec.Code)
	}
}

func TestAdminBackends(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/backends", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("后端清单期望 2 项，实际 %d", len(out))
	}
}

func TestCordonUncordon(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "POST", "/admin/backends/b1/cordon", ""); rec.Code != 200 {
		t.Fatalf("cordon 期望 200，实际 %d", rec.Code)
	}
	if !s.reg.Get("b1").Cordoned() {
		t.Fatal("b1 应处于隔离状态")
	}
	if rec := do(t, s, "POST", "/admin/backends/b1/uncordon", ""); rec.Code != 200 {
		t.Fatalf("uncordon 期望 200，实际 %d", rec.Code)
	}
	if s.reg.Get("b1").Cordoned() {
		t.Fatal("b1 应已解除隔离")
	}
	if rec := do(t, s, "POST", "/admin/backends/nope/cordon", ""); rec.Code != 404 {
		t.Fatalf("不存在的后端应 404，实际 %d", rec.Code)
	}
}

func TestPolicyHotUpdate(t *testing.T) {
	s := newTestServer(t)
	// 合法更新生效。
	rec := do(t, s, "PUT", "/admin/policies/default", `{"filter":"healthy","score":"waiting * 3.0"}`)
	if rec.Code != 200 {
		t.Fatalf("合法策略更新应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.pol.Get("default").ScoreSrc; got != "waiting * 3.0" {
		t.Fatalf("策略未生效: %s", got)
	}
	// 非法表达式拒绝且不影响运行策略。
	rec = do(t, s, "PUT", "/admin/policies/default", `{"score":"waiting *"}`)
	if rec.Code != 400 {
		t.Fatalf("非法表达式应 400，实际 %d", rec.Code)
	}
	if got := s.pol.Get("default").ScoreSrc; got != "waiting * 3.0" {
		t.Fatal("编译失败不应影响运行中的策略")
	}
}

func TestExplain(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/explain?model=m1&prompt=hello", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var out struct {
		Scores []map[string]interface{} `json:"scores"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Scores) != 2 {
		t.Fatalf("打分明细期望 2 项，实际 %d", len(out.Scores))
	}
}

func TestAlertsDisabled(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/alerts", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "false") {
		t.Fatalf("未启用告警时应返回 enabled=false: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "GET", "/admin/autoscale", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "false") {
		t.Fatalf("未启用扩缩容时应返回 enabled=false: %d %s", rec.Code, rec.Body.String())
	}
}
