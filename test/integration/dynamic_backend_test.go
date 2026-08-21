// 动态后端注册端到端测试：admin POST/DELETE 在本实例生效并经集群广播
// 同步到其余实例（持久层路径由 internal/store 单测覆盖，此处不接数据库）。
package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/test/mockbackend"
)

// backendsSeen 连发 n 次请求，收集实际承接的后端 ID 集合。
func backendsSeen(t *testing.T, g *gw, n int) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		code, id := doChat(g, "")
		if code != http.StatusOK {
			t.Fatalf("请求失败: %d", code)
		}
		seen[id] = true
	}
	return seen
}

func TestDynamicBackendAddRemove(t *testing.T) {
	g1, g2 := twoGateways(t, nil)

	// 第三个 mock 后端，运行期经 gw-1 的 admin 注册。
	m3 := mockbackend.New("vllm")
	srv3 := httptest.NewServer(m3.Handler())
	t.Cleanup(srv3.Close)

	payload := `{"id":"b3","url":"` + srv3.URL + `","engine":"vllm","models":["m1"]}`
	req := httptest.NewRequest("POST", "/admin/backends", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer it-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g1.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("注册后端失败: %d %s", rec.Code, rec.Body.String())
	}

	// gw-1 立即可路由到 b3（round_robin 轮转全池）。
	if seen := backendsSeen(t, g1, 6); !seen["b3"] {
		t.Fatalf("gw-1 未把流量路由到新后端: %v", seen)
	}
	// gw-2 经广播同步后也应路由到 b3。
	waitFor(t, 2*time.Second, func() bool {
		return backendsSeen(t, g2, 6)["b3"]
	}, "gw-2 未收到后端注册广播")

	// 摘除后两个实例都不再路由到 b3。
	req = httptest.NewRequest("DELETE", "/admin/backends/b3", nil)
	req.Header.Set("Authorization", "Bearer it-token")
	rec = httptest.NewRecorder()
	g1.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("摘除后端失败: %d %s", rec.Code, rec.Body.String())
	}
	if seen := backendsSeen(t, g1, 6); seen["b3"] {
		t.Fatalf("gw-1 摘除后仍路由到 b3: %v", seen)
	}
	waitFor(t, 2*time.Second, func() bool {
		return !backendsSeen(t, g2, 6)["b3"]
	}, "gw-2 未收到后端摘除广播")
}

func TestDynamicBackendValidation(t *testing.T) {
	g1, _ := twoGateways(t, nil)

	for name, tc := range map[string]struct {
		payload string
		want    int
	}{
		"缺少模型":  {`{"id":"x","url":"http://127.0.0.1:1","engine":"vllm"}`, http.StatusBadRequest},
		"引擎非法":  {`{"id":"x","url":"http://127.0.0.1:1","engine":"tgi","models":["m1"]}`, http.StatusBadRequest},
		"路由不存在": {`{"id":"x","url":"http://127.0.0.1:1","engine":"vllm","models":["nope"]}`, http.StatusBadRequest},
		"重复 ID": {`{"id":"b1","url":"http://127.0.0.1:1","engine":"vllm","models":["m1"]}`, http.StatusConflict},
	} {
		req := httptest.NewRequest("POST", "/admin/backends", strings.NewReader(tc.payload))
		req.Header.Set("Authorization", "Bearer it-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		g1.handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("[%s] 期望 %d，实际 %d: %s", name, tc.want, rec.Code, rec.Body.String())
		}
	}
}
