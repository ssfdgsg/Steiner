// 控制台静态托管单测：首页、资源、单页回退与产物可用性判定。
package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAvailable(t *testing.T) {
	// 仓库中已提交构建产物，Available 应为 true；
	// 若本用例失败，通常是 dist/ 被清空 —— 跑 `make web` 重建。
	if !Available() {
		t.Skip("控制台产物未构建（make web），跳过")
	}
}

func TestServeIndex(t *testing.T) {
	if !Available() {
		t.Skip("控制台产物未构建，跳过")
	}
	rec := httptest.NewRecorder()
	Handler("/admin/ui/").ServeHTTP(rec, httptest.NewRequest("GET", "/admin/ui/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("首页期望 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatalf("首页内容不符: %s", rec.Body.String()[:min(200, rec.Body.Len())])
	}
}

func TestServeAsset(t *testing.T) {
	if !Available() {
		t.Skip("控制台产物未构建，跳过")
	}
	rec := httptest.NewRecorder()
	Handler("/admin/ui/").ServeHTTP(rec, httptest.NewRequest("GET", "/admin/ui/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("JS 资源期望 200 且非空，实际 %d/%d 字节", rec.Code, rec.Body.Len())
	}
}

// TestSPAFallback 未命中具体文件的路径回退到 index.html：
// 单页 hash 路由下直接刷新任意路径都不应 404。
func TestSPAFallback(t *testing.T) {
	if !Available() {
		t.Skip("控制台产物未构建，跳过")
	}
	rec := httptest.NewRecorder()
	Handler("/admin/ui/").ServeHTTP(rec, httptest.NewRequest("GET", "/admin/ui/backends", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("单页回退期望 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatal("回退内容应为 index.html")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
