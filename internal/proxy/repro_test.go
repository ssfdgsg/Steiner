// 复现性单测：围绕功能漏洞审查（docs/functional-vulnerability-review.md）中
// M4 与 H12 三条 proxy 层缺陷，均修复并翻转断言：
//
//   - TestReproM4A_4xxClearsFaultCounter：修复后——400 不再 MarkSuccess 清计数
//   - TestReproM4B_4xxBindsSession：修复后——400 不再绑定会话
//   - TestReproH12_ClientDisconnectMarksBackendFailure：修复后——客户端断开不再
//     记为后端失败、不再以已取消 ctx 继续重试（一次断开不再把两个后端都误熔断）
package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/session"
	"ai-gateway/test/mockbackend"
)

// TestReproM4A_4xxClearsFaultCounter 复现并验证 M4 之一：后端返回 400 时，
// tryForward 不再走 `code < 500 → b.MarkSuccess()` 分支（proxy.go 已改为
// `code < 400`），4xx 不再算"成功"，不会把连续失败计数清零。
//
// 断言方式：Backend.consecutiveFails 不可导出，故用"探针"间接观察——
// 先把计数推到 2（阈值 3，不摘除），经网关转发回 400；修复后（4xx 不再
// MarkSuccess）计数仍为 2，该探针触发摘除，断言翻转（原缺陷行为：4xx 清零
// 计数，探针后 count=1 不达阈值、后端保持可用）。
func TestReproM4A_4xxClearsFaultCounter(t *testing.T) {
	// 自定义上游：固定回 400 + JSON 错误体（如内容审核拒绝、per-token 超限）。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"content rejected","type":"invalid_request_error","code":400}}`)
	}))
	defer upstream.Close()

	st := newStack(t, &config.Config{
		Server: config.ServerConfig{
			FailureThreshold: 3, // 网关 MarkFailure 用的阈值（4xx 路径不触发）
			EjectCooldown:    config.Duration(30 * time.Second),
		},
		Backends: []config.BackendConfig{{ID: "b1", URL: upstream.URL, Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"b1"}}},
	})
	b1 := st.reg.Get("b1")
	if b1 == nil {
		t.Fatal("注册表未找到 b1")
	}

	// 手动把连续失败计数推到 2（< 阈值 3，不应触发摘除）。
	b1.MarkFailure(3, 30*time.Second)
	b1.MarkFailure(3, 30*time.Second)
	if b1.Ejected(time.Now()) {
		t.Fatal("前置断言失败：2 次失败（阈值 3）不应摘除")
	}

	// 经网关转发，上游回 400：修复后 4xx 透传但不清零失败计数。
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("上游 400 应原样透传，实际 %d", rec.Code)
	}

	// 修复后断言：400 未清除失败计数 → 探针 MarkFailure(3) 后计数达 3，后端被摘除。
	b1.MarkFailure(3, 30*time.Second)
	if !b1.Ejected(time.Now()) {
		t.Fatal("M4 修复失败：400 不应调用 MarkSuccess 清零失败计数，" +
			"探针 MarkFailure(3) 后计数应达 3 触发摘除")
	}
	if b1.Available(time.Now()) {
		t.Fatal("M4 修复失败：摘除后后端不应可参与调度")
	}
}

// TestReproM4B_4xxBindsSession 复现并验证 M4 之二：后端返回 400 时，
// 不再执行会话（重新）绑定（原缺陷：`code < 500` 分支在 4xx 也写 Bind，
// 一个持续拒绝该会话的后端会被持续"粘住"）。
//
// 断言方式：注入 session.Store，least_request 同负载先选 b1；b1 回 400 后
// 查询会话绑定。修复后（4xx 不绑定）Lookup 应未命中，断言翻转。
func TestReproM4B_4xxBindsSession(t *testing.T) {
	// b1 拒绝该会话（400），b2 正常应答（200）。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"per-token limit exceeded","type":"invalid_request_error","code":400}}`)
	}))
	defer bad.Close()
	good := mockbackend.New("vllm")
	goodSrv := httptest.NewServer(good.Handler())
	defer goodSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: bad.URL, Engine: "vllm"},
			{ID: "b2", URL: goodSrv.URL, Engine: "vllm"},
		},
		// least_request：同负载取池内第一个 → b1（回 400 的那个）。
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}, Strategy: "least_request"}},
	})
	sess := session.NewStore(time.Minute, 1000)
	st.handler.SetSessionStore(sess)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "sess-4xx")
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("上游 400 应原样透传，实际 %d", rec.Code)
	}

	// 修复后断言：400 透传但不写会话绑定 → Lookup 应未命中。
	got, ok := sess.Lookup("sess-4xx")
	if ok {
		t.Fatalf("M4 修复失败：400 不应绑定会话到任何后端，实际绑定到 %q", got)
	}
}

// TestReproH12_ClientDisconnectMarksBackendFailure 复现并验证 H12：客户端在网关
// 已向上游发出请求后断开（ctx 取消）。doUpstream 返回 context.Canceled——修复后
// 不再执行 b.MarkFailure（proxy.go 错误分支先判 ctx 取消），也不再在已取消的 ctx
// 上继续换后端重试（proxy.go:356-367 的循环因 done=true 直接返回）。
//
// 断言方式：两个上游 handler 都阻塞永不返回；等第一条请求真正打到 b1 后取消
// 客户端 ctx。FailureThreshold=1/Retries=2 设置保留——修复后一次断开不应把
// 任何后端误熔断、不应产生重试（b2 保持 0 次连接），响应仍为 503 类。
func TestReproH12_ClientDisconnectMarksBackendFailure(t *testing.T) {
	var hitsB1, hitsB2 atomic.Int32
	block := make(chan struct{}) // 永不关闭 → 上游 handler 永不返回
	up1Start := make(chan struct{}, 1)
	up2Start := make(chan struct{}, 1)
	blocking := func(hits *atomic.Int32, start chan struct{}) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			select {
			case start <- struct{}{}:
			default:
			}
			<-block
		}
	}
	srv1 := httptest.NewServer(blocking(&hitsB1, up1Start))
	srv2 := httptest.NewServer(blocking(&hitsB2, up2Start))
	// LIFO：先放行卡死的 handler，再关服务器，避免 Close 永久等待。
	defer srv1.Close()
	defer srv2.Close()
	defer close(block)

	st := newStack(t, &config.Config{
		Server: config.ServerConfig{
			Retries:          2, // 默认值，attempts = 3
			FailureThreshold: 1, // 一次失败即摘除，把“被 MarkFailure”变成可断言结果
			EjectCooldown:    config.Duration(30 * time.Second),
		},
		Backends: []config.BackendConfig{
			{ID: "b1", URL: srv1.URL, Engine: "vllm"},
			{ID: "b2", URL: srv2.URL, Engine: "vllm"},
		},
		// least_request：同负载先选 b1，失败后换 b2 重试。
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}, Strategy: "least_request"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		st.handler.ServeHTTP(rec, req)
	}()

	// 等第一条请求真正打到 b1：证明取消发生在“上游请求已发出”之后。
	select {
	case <-up1Start:
	case <-time.After(5 * time.Second):
		t.Fatal("第一条请求未到达上游 b1")
	}

	cancel() // 客户端断开（LB 探活 / SDK 超时 / 关页）

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP 未在取消后及时返回")
	}

	now := time.Now()
	b1, b2 := st.reg.Get("b1"), st.reg.Get("b2")

	// 修复后断言 1：客户端断开 ≠ 后端失败 → b1 保持可用（未被 MarkFailure 摘除）。
	if b1.Ejected(now) || !b1.Available(now) {
		t.Fatal("H12 修复失败：客户端断开不应 MarkFailure b1（后端无辜）")
	}
	// 修复后断言 2：不再重试 → b2 未被波及，同样保持可用。
	if b2.Ejected(now) || !b2.Available(now) {
		t.Fatal("H12 修复失败：客户端断开不应误伤 b2（已取消 ctx 上不应继续重试记账）")
	}
	// 佐证：b1 恰好被真实连接一次（取消发生在连接建立之后）；b2 从未被连接过——
	// 证明不再发生"已取消 ctx 上的重试"。
	if got := hitsB1.Load(); got != 1 {
		t.Fatalf("佐证失败：b1 应恰好被上游连接 1 次，实际 %d", got)
	}
	if got := hitsB2.Load(); got != 0 {
		t.Fatalf("佐证失败：客户端断开后不应继续重试（b2 应 0 次连接），实际 %d", got)
	}
	// 响应仍为 503 类：客户端断开直接以 503 中止并向客户端返回。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("佐证失败：客户端断开中止应 503，实际 %d", rec.Code)
	}
}

// TestReproL2_DeadBindingUnboundOnLookup 复现并验证 L2：Lookup 命中指向已摘除
// 后端的绑定后，代理必须立即解除该死绑定——否则该绑定会被后续每次 Lookup
// 反复滑动续期、占据表容量并淘汰活跃会话（docs/functional-vulnerability-review.md 的 L2）。
//
// 翻转路径：请求整体失败（幸存后端也不可用）→ 不发生成功改绑，绑定残留与否
// 完全由 serveNormal 的失效 Lookup 清理逻辑决定。
// 修复前：死绑定（sess-dead -> b1）在失败路径上驻留并被续期，Lookup 仍命中；
// 修复后：首次失效命中即 Unbind，Lookup 未命中。
func TestReproL2_DeadBindingUnboundOnLookup(t *testing.T) {
	st := newStack(t, &config.Config{
		Server: config.ServerConfig{Retries: 1},
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"}, // 粘性目标：随后被摘除
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "vllm"}, // 幸存后端：也不可用 → 请求失败
		},
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}}},
	})
	sess := session.NewStore(time.Minute, 1000)
	st.handler.SetSessionStore(sess)
	sess.Bind("sess-dead", "b1")

	// RemoveBackend 摘除 b1（健康翻转未触发/未覆盖的路径：详见 L2 触发条件）。
	if err := st.reg.RemoveBackend("b1"); err != nil {
		t.Fatalf("摘除 b1 失败: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req.Header.Set("X-Session-Id", "sess-dead")
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	// 请求整体失败（全部可用后端转发失败），失败路径下不发生成功改绑——
	// 死绑定是否残留即被本测试直接观测。
	if rec.Code < 500 {
		t.Fatalf("前置：期望整体失败（5xx），实际 %d", rec.Code)
	}
	if id, ok := sess.Lookup("sess-dead"); ok {
		t.Fatalf("L2 缺陷：指向已摘除后端的死绑定仍被保留并续期（id=%q），修复后应解除", id)
	}
}

// TestReproL2_DeadBindingUnboundNotAffectingOthers 同场景的对照：失效清理是
// 按会话精准的，未失效的会话绑定保持可用（不被误伤）。
func TestReproL2_DeadBindingUnboundNotAffectingOthers(t *testing.T) {
	mock := mockbackend.New("vllm")
	live := httptest.NewServer(mock.Handler())
	defer live.Close()

	st := newStack(t, &config.Config{
		Server: config.ServerConfig{Retries: 1},
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"}, // 将被摘除
			{ID: "live", URL: live.URL, Engine: "vllm"},
		},
		Models: []config.ModelRoute{{Name: "m1", Backends: []string{"b1", "live"}}},
	})
	sess := session.NewStore(time.Minute, 1000)
	st.handler.SetSessionStore(sess)
	sess.Bind("sess-dead", "b1")
	sess.Bind("sess-alive", "live")

	if err := st.reg.RemoveBackend("b1"); err != nil {
		t.Fatalf("摘除 b1 失败: %v", err)
	}

	// 死会话请求：摘除 b1 后请求落到 live，成功应答使会话改绑到 live——
	// 关键是绑定不再指向死后端 b1。
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req1.Header.Set("X-Session-Id", "sess-dead")
	rec1 := httptest.NewRecorder()
	st.handler.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("摘除 b1 后死会话应回落选路成功，实际 %d", rec1.Code)
	}
	if id, ok := sess.Lookup("sess-dead"); !ok || id == "b1" {
		t.Fatalf("死绑定应被解除/改绑，不应再指向 b1（id=%q ok=%v）", id, ok)
	}

	// 活跃会话：绑定仍指向 live，粘性不受清理逻辑影响。
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req2.Header.Set("X-Session-Id", "sess-alive")
	rec2 := httptest.NewRecorder()
	st.handler.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("活跃会话请求应成功，实际 %d", rec2.Code)
	}
	if id, ok := sess.Lookup("sess-alive"); !ok || id != "live" {
		t.Fatalf("有效绑定应保持（id=%q ok=%v），失效清理不得误伤", id, ok)
	}
	if got := rec2.Header().Get("X-Upstream-Backend"); got != "live" {
		t.Fatalf("活跃会话应粘到 live，实际 %q", got)
	}
}
