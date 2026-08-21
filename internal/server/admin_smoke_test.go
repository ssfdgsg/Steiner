// 管理面其余端点的冒烟单测：提升 server 包覆盖率（57% 基线薄弱）。
// 全部使用 newTestServer/do 助手；断言成功路径与关键错误路径。
package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdminPoliciesGet(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/policies", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "default") {
		t.Fatalf("策略清单异常: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminPresets(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/presets", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "balanced") {
		t.Fatalf("预设清单异常: %d %s", rec.Code, rec.Body.String())
	}
	// 应用合法预设：默认策略更新为 balanced 预设表达式。
	rec = do(t, s, "POST", "/admin/presets/balanced/apply", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应用预设应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 不存在的预设。
	rec = do(t, s, "POST", "/admin/presets/nope/apply", "")
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
		t.Fatalf("不存在预设应 4xx，实际 %d", rec.Code)
	}
}

func TestAdminKVCacheAndPD(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/admin/kvcache", ""); rec.Code != http.StatusOK {
		t.Fatalf("kvcache 视图异常: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/admin/pd", ""); rec.Code != http.StatusOK {
		t.Fatalf("pd 视图异常: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminStatsAndHistory(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/admin/stats", ""); rec.Code != http.StatusOK {
		t.Fatalf("stats 异常: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/admin/stats/history", ""); rec.Code != http.StatusOK {
		t.Fatalf("stats/history 异常: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/admin/stats/history?minutes=60", ""); rec.Code != http.StatusOK {
		t.Fatalf("stats/history?minutes=60 异常: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminQueueClusterRollouts(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/admin/queue", ""); rec.Code != http.StatusOK {
		t.Fatalf("queue 视图异常: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/admin/cluster", ""); rec.Code != http.StatusOK {
		t.Fatalf("cluster 视图异常: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/admin/rollouts", ""); rec.Code != http.StatusOK {
		t.Fatalf("rollouts 视图异常: %d %s", rec.Code, rec.Body.String())
	}
	// 不存在的模型 reset 应 404。
	if rec := do(t, s, "POST", "/admin/rollouts/nope/reset", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的 rollout reset 应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModels(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/models", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "m1") {
		t.Fatalf("模型清单异常: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUI(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/ui/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("控制台首页应 200，实际 %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/admin/ui", ""); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("控制台无斜杠应 301 重定向，实际 %d", rec.Code)
	}
}
