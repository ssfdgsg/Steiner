// 预设方案管理端点测试：清单展示、一键切换、指定策略槽位、未知方案拒绝。
package server

import (
	"encoding/json"
	"testing"

	"ai-gateway/internal/policy"
)

// presetsResp GET /admin/presets 的响应结构。
type presetsResp struct {
	Presets  []policy.Preset              `json:"presets"`
	Policies map[string]map[string]string `json:"policies"`
}

func TestPresetsList(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/presets", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var out presetsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Presets) != len(policy.Presets) {
		t.Fatalf("预设数量不符: %d", len(out.Presets))
	}
	// 前端展示所需字段齐全。
	for _, p := range out.Presets {
		if p.Title == "" || p.Description == "" || p.Score == "" {
			t.Fatalf("预设 %s 展示字段缺失", p.Name)
		}
	}
	// 默认策略与内置 balanced 表达式一致，应反查为 balanced。
	if got := out.Policies["default"]["preset"]; got != "balanced" {
		t.Fatalf("默认策略应反查为 balanced，实际 %q", got)
	}
}

func TestPresetApply(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "POST", "/admin/presets/latency_first/apply", "")
	if rec.Code != 200 {
		t.Fatalf("切换应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}

	pr := policy.FindPreset("latency_first")
	if got := s.pol.Get("default").ScoreSrc; got != pr.Score {
		t.Fatalf("default 策略未切换到 latency_first: %s", got)
	}
	if got := s.pol.Get("default").FilterSrc; got != pr.Filter {
		t.Fatalf("filter 未切换: %s", got)
	}

	// 清单接口应反映新的生效方案。
	var out presetsResp
	_ = json.Unmarshal(do(t, s, "GET", "/admin/presets", "").Body.Bytes(), &out)
	if got := out.Policies["default"]["preset"]; got != "latency_first" {
		t.Fatalf("生效方案应为 latency_first，实际 %q", got)
	}
}

// TestPresetApplyToNamedPolicy ?policy= 指定切换目标槽位，不影响 default。
func TestPresetApplyToNamedPolicy(t *testing.T) {
	s := newTestServer(t)
	before := s.pol.Get("default").ScoreSrc

	rec := do(t, s, "POST", "/admin/presets/throughput_first/apply?policy=batch", "")
	if rec.Code != 200 {
		t.Fatalf("切换应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	pr := policy.FindPreset("throughput_first")
	if s.pol.Get("batch") == nil || s.pol.Get("batch").ScoreSrc != pr.Score {
		t.Fatal("batch 策略槽位未按预设创建")
	}
	if s.pol.Get("default").ScoreSrc != before {
		t.Fatal("指定槽位切换不应影响 default")
	}
}

func TestPresetApplyUnknown(t *testing.T) {
	s := newTestServer(t)
	before := s.pol.Get("default").ScoreSrc
	rec := do(t, s, "POST", "/admin/presets/不存在的方案/apply", "")
	if rec.Code != 404 {
		t.Fatalf("未知预设应 404，实际 %d", rec.Code)
	}
	if s.pol.Get("default").ScoreSrc != before {
		t.Fatal("失败的切换不应改动运行策略")
	}
}

// TestPresetApplyAllPresets 全部预设都能经端点切换成功（防止新增预设漏测）。
func TestPresetApplyAllPresets(t *testing.T) {
	s := newTestServer(t)
	for _, pr := range policy.Presets {
		rec := do(t, s, "POST", "/admin/presets/"+pr.Name+"/apply", "")
		if rec.Code != 200 {
			t.Fatalf("预设 %s 切换失败: %d %s", pr.Name, rec.Code, rec.Body.String())
		}
		if s.pol.Get("default").ScoreSrc != pr.Score {
			t.Fatalf("预设 %s 未生效", pr.Name)
		}
	}
}

// TestConsoleUI 控制台静态资源经网关路由可访问，且单页回退生效。
func TestConsoleUI(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/admin/ui/", ""); rec.Code != 200 {
		t.Fatalf("控制台首页期望 200，实际 %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/admin/ui/assets/app.js", ""); rec.Code != 200 {
		t.Fatalf("控制台 JS 资源期望 200，实际 %d", rec.Code)
	}
	// 不带尾斜杠应重定向（保证页面内相对资源路径正确）。
	if rec := do(t, s, "GET", "/admin/ui", ""); rec.Code != 301 {
		t.Fatalf("/admin/ui 期望 301 重定向，实际 %d", rec.Code)
	}
}
