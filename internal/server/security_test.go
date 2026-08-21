// 安全面单测：验证 C1（admin 无鉴权）与 H1（Content-Type 不校验）修复后的行为。
//
// 修复内容：①管理面强制 Bearer 令牌（未授权一律 401）；②变更类方法强制
// Content-Type: application/json（非 JSON 一律 415，切断跨站表单 CSRF 前置）。
// 这些测试从"复现缺陷"翻转而来：修复前断言 201/2xx（缺陷存在），修复后断言 401/415。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const adminToken = "test-token"

// authReq 构造带/不带令牌与指定 Content-Type 的管理面请求。
func authReq(method, path, body, token, contentType string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// TestC1AdminNoAuthRejected 修复后断言：无凭据（或无正确令牌）的
// POST /admin/backends 一律 401，后端不会被注册。
func TestC1AdminNoAuthRejected(t *testing.T) {
	s := newTestServer(t)
	body := `{"id":"evil","url":"http://attacker.invalid/","engine":"vllm","models":["m1"]}`
	cases := []struct {
		name  string
		token string
	}{
		{"无 Authorization 头", ""},
		{"错误令牌", "wrong-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authReq(http.MethodPost, "/admin/backends", body, tc.token, "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("修复后未授权注册应 401，实际 %d: %s", rec.Code, rec.Body.String())
			}
			if s.reg.Get("evil") != nil {
				t.Fatal("修复后未授权注册不得进入注册表")
			}
		})
	}
}

// TestC1AdminAuthSuccess 正向控制：携带正确令牌 + JSON 头时注册仍可用。
func TestC1AdminAuthSuccess(t *testing.T) {
	s := newTestServer(t)
	req := authReq(http.MethodPost, "/admin/backends",
		`{"id":"ok","url":"http://ok.invalid/","engine":"vllm","models":["m1"]}`,
		adminToken, "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("携带正确令牌应 201，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if s.reg.Get("ok") == nil {
		t.Fatal("携带正确令牌的后端应进入注册表")
	}
}

// TestC1AdminNoAuthDeleteRejected 修复后断言：无凭据 DELETE 摘除后端一律 401。
func TestC1AdminNoAuthDeleteRejected(t *testing.T) {
	s := newTestServer(t)
	// 用正确令牌先注册 victim。
	req := authReq(http.MethodPost, "/admin/backends",
		`{"id":"victim","url":"http://victim.invalid/","engine":"vllm","models":["m1"]}`,
		adminToken, "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("前置注册失败: %d", rec.Code)
	}
	// 无凭据摘除 → 401 且不影响注册表。
	req = authReq(http.MethodDelete, "/admin/backends/victim", "", "", "")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("修复后未授权摘除应 401，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if s.reg.Get("victim") == nil {
		t.Fatal("修复后未授权摘除不得生效")
	}
}

// TestH1NonJSONContentTypeRejected 修复后断言：携带正确令牌但 Content-Type
// 非 application/json（如跨域 fetch no-cors 可发送的 text/plain）→ 415。
func TestH1NonJSONContentTypeRejected(t *testing.T) {
	s := newTestServer(t)
	body := `{"id":"csrf","url":"http://evil.invalid/","engine":"vllm","models":["m1"]}`
	// text/plain 与缺省 Content-Type 两种形态均须拒绝。
	for _, ct := range []string{"text/plain", ""} {
		req := authReq(http.MethodPost, "/admin/backends", body, adminToken, ct)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("修复后非 JSON Content-Type(%q) 应 415，实际 %d: %s", ct, rec.Code, rec.Body.String())
		}
		if s.reg.Get("csrf") != nil {
			t.Fatal("修复后非 JSON 请求不得生效")
		}
	}
}

// TestH1SuccessPathContentType 正向控制：NoBody 的 DELETE 无需 Content-Type。
func TestH1SuccessPathContentType(t *testing.T) {
	s := newTestServer(t)
	req := authReq(http.MethodGet, "/admin/backends", "", adminToken, "")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 携带令牌应 200，实际 %d", rec.Code)
	}
}

// TestAdminHealthzMetricsUnauthenticated 非管理面端点不受令牌保护。
func TestAdminHealthzMetricsUnauthenticated(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz 应放行，实际 %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/metrics", ""); rec.Code != http.StatusOK {
		t.Fatalf("metrics 应放行，实际 %d", rec.Code)
	}
}

// TestConsoleShellPublicDataProtected 控制台静态壳公开（SPA 先加载后登录），
// admin 数据端点仍全部受 Bearer 保护（README"开箱即用"的成立前提）。
func TestConsoleShellPublicDataProtected(t *testing.T) {
	s := newTestServer(t)
	// 无令牌也能加载控制台页面与静态资源。
	for _, p := range []string{"/admin/ui/", "/admin/ui/assets/app.js"} {
		rec := do(t, s, "GET", p, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("控制台静态壳 %s 应公开可加载，实际 %d", p, rec.Code)
		}
	}
	// 无令牌访问 admin 数据端点仍 401（数据不因控制台公开而泄露）。
	// 注意：do() 助手自动携带 Bearer + JSON 头，此处须用 authReq 构造真无凭据请求。
	for _, p := range []string{"/admin/backends", "/admin/models", "/admin/stats", "/admin/policies/default"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, authReq(http.MethodGet, p, "", "", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("数据端点 %s 无令牌应 401，实际 %d", p, rec.Code)
		}
	}
}
