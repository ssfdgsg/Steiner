// PD 转发缺陷修复复现测试（对 docs/functional-vulnerability-review.md H3/H4/M12）：
//   - H3：decode 返回 >=500 且尚未写出首字节 → 换 decode 重试并成功；
//     全部 decode 均失败 → 透传最后一个 503；
//   - H4：sglang prefill 孤儿双发与误熔断——decode 侧失败重试前旧 prefill
//     请求随尝试结束取消并退出（不并发双发、释放额度）；被抛弃的旧请求超时
//     不把健康 prefill 记失败；客户端断开不记账、不重试（H12 语义）；
//   - M12：vllm prefill 响应超过 4MiB 被 LimitReader 截断 → 打 Warn 日志，
//     请求继续按截断数据处理（行为不变，只新增告警）。
package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/session"
	"ai-gateway/test/mockbackend"
)

// TestPDDecode5xxRetriesToNextDecode H3：第一个 decode 固定回 503，
// 请求应被重试到第二个 decode 并成功，两个 decode 各被请求一次。
func TestPDDecode5xxRetriesToNextDecode(t *testing.T) {
	prefill := mockbackend.New("vllm")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	badDecode := mockbackend.New("vllm")
	badDecode.SetFault(mockbackend.Fault{FailCode: http.StatusServiceUnavailable})
	badSrv := httptest.NewServer(badDecode.Handler())
	defer badSrv.Close()
	goodDecode := mockbackend.New("vllm")
	goodSrv := httptest.NewServer(goodDecode.Handler())
	defer goodSrv.Close()

	st := newPDStack(t, "vllm",
		[]*httptest.Server{prefillSrv},
		[]*httptest.Server{badSrv, goodSrv}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200（应排除坏 decode 重试到好 decode），实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "模拟应答") {
		t.Fatalf("响应体不符: %s", rec.Body.String())
	}
	// 全互联下 Pick 稳定先选 d1（坏）：d1 应恰好被请求 1 次，触发重试后 d2 承接。
	if badDecode.ReqCount() != 1 {
		t.Fatalf("坏 decode 应恰好被请求 1 次，实际 %d", badDecode.ReqCount())
	}
	if goodDecode.ReqCount() != 1 {
		t.Fatalf("好 decode 应恰好被请求 1 次，实际 %d", goodDecode.ReqCount())
	}
}

// TestPDAllDecodesFailPassthroughLast503 H3：全部 decode 固定回 503 且重试
// 次数耗尽（Retries=1 → 共 2 次尝试）时，客户端收到最后一个 503 而非网关 502。
func TestPDAllDecodesFailPassthroughLast503(t *testing.T) {
	prefill := mockbackend.New("vllm")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	d1 := mockbackend.New("vllm")
	d1.SetFault(mockbackend.Fault{FailCode: http.StatusServiceUnavailable})
	d1Srv := httptest.NewServer(d1.Handler())
	defer d1Srv.Close()
	d2 := mockbackend.New("vllm")
	d2.SetFault(mockbackend.Fault{FailCode: http.StatusServiceUnavailable})
	d2Srv := httptest.NewServer(d2.Handler())
	defer d2Srv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "p1", URL: prefillSrv.URL, Engine: "vllm"},
			{ID: "d1", URL: d1Srv.URL, Engine: "vllm"},
			{ID: "d2", URL: d2Srv.URL, Engine: "vllm"},
		},
		PDGroups: []config.PDGroupConfig{{
			Name: "g1", Prefill: []string{"p1"}, Decode: []string{"d1", "d2"},
		}},
		Models: []config.ModelRoute{{Name: "m1", PDGroup: "g1"}},
		// 2 次尝试：d1、d2 各失败一次后耗尽，走 servePD 循环末尾的透传分支。
		Server: config.ServerConfig{Retries: 1},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("全部 decode 失败应透传最后一个 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if d1.ReqCount() != 1 || d2.ReqCount() != 1 {
		t.Fatalf("两个 decode 应各被请求一次: d1=%d d2=%d", d1.ReqCount(), d2.ReqCount())
	}
}

// TestPDVLLMPrefillTruncationWarns M12：vllm prefill 响应超过 4MiB 被
// LimitReader 截断时打 Warn 日志，请求继续按截断数据处理（decode 无 KV 句柄）。
func TestPDVLLMPrefillTruncationWarns(t *testing.T) {
	// prefill 返回 >4MiB 响应：合法 JSON 头 + 4MiB 填充，截断点落在 JSON 中途，
	// 与真实场景一致（kv_transfer_params 落在被丢弃的尾部）。
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"kv_transfer_params":{"remote_host":"127.0.0.1",`)
		_, _ = w.Write(bytes.Repeat([]byte(" "), 4<<20))
		io.WriteString(w, `"remote_block_ids":[1,2,3]}}`)
	}))
	defer prefillSrv.Close()
	decode := mockbackend.New("vllm")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	// 捕获网关日志（TextHandler 内部带锁，并发写安全）。
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	st := newPDStack(t, "vllm", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("截断后请求应继续按截断数据处理，期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), "prefill 响应超过 4MiB 被截断,可能丢失 kv_transfer_params") {
		t.Fatalf("应打截断 Warn 日志，实际日志:\n%s", buf.String())
	}
	// 截断导致 json.Unmarshal 失败 → ktp=nil → decode 请求不再携带
	// kv_transfer_params（与修复前行为一致，本次仅新增告警）。
	if db := decode.LastBody(); db != nil {
		if _, ok := db["kv_transfer_params"]; ok {
			t.Fatalf("截断后 decode 不应携带 kv_transfer_params: %v", db)
		}
	}
}

// TestPDVLLMPrefillNoWarnAtExactLimit M12 反例：响应恰为 4MiB 时不应告警。
func TestPDVLLMPrefillNoWarnAtExactLimit(t *testing.T) {
	// 恰好 4MiB 的合法 JSON：LimitReader 读满即 EOF，无截断。
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		base := `{"id":"x","object":"chat.completion","choices":[],"kv_transfer_params":{"remote_host":"127.0.0.1"}}`
		// 填充到恰好 4MiB。
		payload := []byte(base)
		if n := 4<<20 - len(payload); n > 0 {
			payload = append(payload, bytes.Repeat([]byte(" "), n)...)
		}
		_, _ = w.Write(payload)
	}))
	defer prefillSrv.Close()
	decode := mockbackend.New("vllm")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	st := newPDStack(t, "vllm", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(buf.String(), "prefill 响应超过 4MiB 被截断") {
		t.Fatalf("恰为 4MiB 不应告警截断，实际日志:\n%s", buf.String())
	}
}

// TestPDSGLangRetryCancelsOrphanPrefill H4：decode 侧 503 触发换对重试时，
// 旧 prefill goroutine 必须随尝试结束被取消并退出（释放并发额度）——重试才能
// 在旧请求终止后安全复用同一 prefill（不并发双发），最终成功返回。
// 修复前：旧 prefill 用独立 background ctx 继续占着额度，MaxConcurrency=1 下
// 重试 TryAcquire 失败 → 502，prefill 只被请求 1 次（重试发不出去）。
// 修复后：旧请求取消退出释放额度，重试顺序重发 → 200，prefill 恰好被请求 2 次。
func TestPDSGLangRetryCancelsOrphanPrefill(t *testing.T) {
	// prefill 模拟长请求（prefill+proposal）：600ms 内不应答。
	prefill := mockbackend.New("sglang")
	prefill.SetFault(mockbackend.Fault{TTFTDelayMS: 600})
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	badDecode := mockbackend.New("sglang")
	badDecode.SetFault(mockbackend.Fault{FailCode: http.StatusServiceUnavailable})
	badSrv := httptest.NewServer(badDecode.Handler())
	defer badSrv.Close()
	goodDecode := mockbackend.New("sglang")
	goodSrv := httptest.NewServer(goodDecode.Handler())
	defer goodSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "p1", URL: prefillSrv.URL, Engine: "sglang", MaxConcurrency: 1},
			{ID: "d1", URL: badSrv.URL, Engine: "sglang"},
			{ID: "d2", URL: goodSrv.URL, Engine: "sglang"},
		},
		PDGroups: []config.PDGroupConfig{{
			Name: "g1", Prefill: []string{"p1"}, Decode: []string{"d1", "d2"},
		}},
		Models: []config.ModelRoute{{Name: "m1", PDGroup: "g1"}},
		// 2 次尝试：d1 503 → 排除 d1 后重试 d2。
		Server: config.ServerConfig{Retries: 1},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("重试应复用同一 prefill 成功（旧请求已随尝试结束取消），期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if badDecode.ReqCount() != 1 || goodDecode.ReqCount() != 1 {
		t.Fatalf("decode 应各被请求一次: d1=%d d2=%d", badDecode.ReqCount(), goodDecode.ReqCount())
	}
	// 两次尝试均落到池内唯一 prefill，且第二次是旧请求终止后的顺序重发（不并发双发）。
	waitFor(t, 2*time.Second, func() bool { return prefill.ReqCount() == 2 },
		"prefill 应恰好被顺序请求两次（旧请求终止后才重发）")
}

// TestPDSGLangOrphanTimeoutNotMarkingHealthyPrefill H4：decode 侧 503 触发
// 重试时被抛弃的旧 prefill 请求（健康但慢、未在预算内应答）不得把健康 prefill
// 记失败/熔断。修复前旧 goroutine 不随尝试结束取消，预算超时后 MarkFailure，
// FailureThreshold=1 时直接把健康 prefill 熔断 15s。
func TestPDSGLangOrphanTimeoutNotMarkingHealthyPrefill(t *testing.T) {
	// 健康但慢的 prefill：1.5s 才应答；预算 500ms 内不应答。
	prefill := mockbackend.New("sglang")
	prefill.SetFault(mockbackend.Fault{TTFTDelayMS: 1500})
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	badDecode := mockbackend.New("sglang")
	badDecode.SetFault(mockbackend.Fault{FailCode: http.StatusServiceUnavailable})
	badSrv := httptest.NewServer(badDecode.Handler())
	defer badSrv.Close()
	goodDecode := mockbackend.New("sglang")
	goodSrv := httptest.NewServer(goodDecode.Handler())
	defer goodSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "p1", URL: prefillSrv.URL, Engine: "sglang"},
			{ID: "d1", URL: badSrv.URL, Engine: "sglang"},
			{ID: "d2", URL: goodSrv.URL, Engine: "sglang"},
		},
		PDGroups: []config.PDGroupConfig{{
			Name: "g1", Prefill: []string{"p1"}, Decode: []string{"d1", "d2"},
		}},
		Models: []config.ModelRoute{{Name: "m1", PDGroup: "g1"}},
		Server: config.ServerConfig{
			Retries:                 1,
			FailureThreshold:        1, // 任何一次误记失败都会直接熔断
			UpstreamResponseTimeout: config.Duration(500 * time.Millisecond),
		},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("decode 重试路径应最终成功，期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 等修复前旧 goroutine 的 500ms 预算超时窗口过去，再断言健康 prefill 未被误熔断。
	time.Sleep(900 * time.Millisecond)
	if p1 := st.reg.Get("p1"); p1.Ejected(time.Now()) {
		t.Fatal("健康 prefill 不应因被抛弃的旧请求预算超时而被误熔断")
	}
}

// TestPDClientDisconnectNoFakeBreachNoRetry H4（H12 语义）：客户端在 prefill/
// decode 均在途时断开。修复前：decode 侧把 context.Canceled 当后端失败记账
// （d1 被熔断），servePD 在已取消 ctx 上继续重试（选中健康 d2 又误记失败、
// 再次双发 prefill 孤儿），prefill 旧 goroutine 用独立 background ctx 不受
// 断开影响、预算超时后把健康 p1 误熔断。修复后：prefill/decode 均不记账、
// 不重试，p1 恰好只被请求一次。
func TestPDClientDisconnectNoFakeBreachNoRetry(t *testing.T) {
	prefill := mockbackend.New("sglang")
	prefill.SetFault(mockbackend.Fault{TTFTDelayMS: 2000})
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	slowDecode := mockbackend.New("sglang")
	slowDecode.SetFault(mockbackend.Fault{TTFTDelayMS: 2000})
	slowSrv := httptest.NewServer(slowDecode.Handler())
	defer slowSrv.Close()
	spareDecode := mockbackend.New("sglang")
	spareSrv := httptest.NewServer(spareDecode.Handler())
	defer spareSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "p1", URL: prefillSrv.URL, Engine: "sglang"},
			{ID: "d1", URL: slowSrv.URL, Engine: "sglang"},
			{ID: "d2", URL: spareSrv.URL, Engine: "sglang"},
		},
		PDGroups: []config.PDGroupConfig{{
			Name: "g1", Prefill: []string{"p1"}, Decode: []string{"d1", "d2"},
		}},
		Models: []config.ModelRoute{{Name: "m1", PDGroup: "g1"}},
		Server: config.ServerConfig{
			Retries:                 2, // 修复前会在已取消 ctx 上重试满 3 次
			FailureThreshold:        1,
			UpstreamResponseTimeout: config.Duration(1000 * time.Millisecond),
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	doneCh := make(chan struct{})
	go func() {
		st.handler.ServeHTTP(rec, req)
		close(doneCh)
	}()
	// 等 prefill 与 decode 双发均已在途后断开客户端。
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后 handler 未及时返回")
	}
	// 等修复前 prefill 旧 goroutine 的 1000ms 预算超时窗口过去，再断言各后端
	// 均未被误记失败/熔断。
	time.Sleep(1200 * time.Millisecond)

	now := time.Now()
	if p1 := st.reg.Get("p1"); p1.Ejected(now) {
		t.Fatal("客户端断开不应导致健康 prefill 被误熔断")
	}
	if d1 := st.reg.Get("d1"); d1.Ejected(now) {
		t.Fatal("客户端断开不应导致 decode d1 被误记失败/熔断")
	}
	if d2 := st.reg.Get("d2"); d2.Ejected(now) {
		t.Fatal("客户端断开不应导致未参与的健康 decode d2 被重试误熔断")
	}
	// 修复前重试会再次双发 prefill；修复后链路随客户端断开中止，p1 只请求一次。
	if got := prefill.ReqCount(); got != 1 {
		t.Fatalf("客户端断开后 prefill 应只被请求 1 次（不重试不双发），实际 %d", got)
	}
}

// ——— H9：PD 路径容量排队与会话粘性 ———

// TestH9PDAdmissionRejectedWhenNoCapacityAndFailClosed 复现并翻转：PD 全无可用
// 对 + 启用了排队与容量准入（表达式恒假）时，请求走准入拒绝（429）。
// 修复前：servePD 完全绕过准入，直接按"无可用对"返回 503——准入形同虚设。
func TestH9PDAdmissionRejectedWhenNoCapacityAndFailClosed(t *testing.T) {
	prefill := mockbackend.New("vllm")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	decode := mockbackend.New("vllm")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	st := newPDStack(t, "vllm", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)
	// 全部 prefill 熔断摘除 → 无可用对 → 触发准入求值。
	st.reg.Get("p1").MarkFailure(1, time.Minute)
	// 装配排队 + 恒假准入（拒绝）。
	st.handler.SetQueue(queue.New(16, 50*time.Millisecond))
	prog, err := policy.CompileBool("false")
	if err != nil {
		t.Fatalf("编译准入表达式失败: %v", err)
	}
	st.handler.SetAdmission(prog, "false")

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("准入拒绝应 429，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 容量恢复后同一准入配置仍放行（准入表达式由集群水位驱动，此处验证取到
	// 的是"拒绝"分支而非 PD 全错 503 分支）。
	if !strings.Contains(rec.Body.String(), "准入") && !strings.Contains(rec.Body.String(), "容量") {
		t.Fatalf("应为准入拒绝文案，实际: %s", rec.Body.String())
	}
}

// TestH9PDSessionStickinessToPrefill 复现并翻转：同一会话连续请求固定命中同一
// prefill（修复前 PD 全无粘性，请求在 prefill 池间漂移）。
func TestH9PDSessionStickinessToPrefill(t *testing.T) {
	p1 := mockbackend.New("vllm")
	p1Srv := httptest.NewServer(p1.Handler())
	defer p1Srv.Close()
	p2 := mockbackend.New("vllm")
	p2Srv := httptest.NewServer(p2.Handler())
	defer p2Srv.Close()
	d1 := mockbackend.New("vllm")
	d1Srv := httptest.NewServer(d1.Handler())
	defer d1Srv.Close()

	st := newPDStack(t, "vllm",
		[]*httptest.Server{p1Srv, p2Srv}, []*httptest.Server{d1Srv}, nil)
	store := session.NewStore(10*time.Minute, 1000)
	st.handler.SetSessionStore(store)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		req.Header.Set("X-Session-Id", "sess-1")
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("请求 %d 应 200，实际 %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	// 修复前：3 次由调度器在 p1/p2 间分配（无粘性）；修复后：固定同一 prefill。
	if (p1.ReqCount() == 3 && p2.ReqCount() == 0) || (p1.ReqCount() == 0 && p2.ReqCount() == 3) {
		return
	}
	t.Fatalf("同一会话应固定命中同一 prefill，实际 p1=%d p2=%d", p1.ReqCount(), p2.ReqCount())
}

// TestH9PDSessionUnbindSelfHeal 绑定 prefill 被熔断摘除后：下一次请求回退并
// 解除绑定（自愈），后续请求稳定落到新 prefill。
func TestH9PDSessionUnbindSelfHeal(t *testing.T) {
	p1 := mockbackend.New("vllm")
	p1Srv := httptest.NewServer(p1.Handler())
	defer p1Srv.Close()
	p2 := mockbackend.New("vllm")
	p2Srv := httptest.NewServer(p2.Handler())
	defer p2Srv.Close()
	d1 := mockbackend.New("vllm")
	d1Srv := httptest.NewServer(d1.Handler())
	defer d1Srv.Close()

	st := newPDStack(t, "vllm",
		[]*httptest.Server{p1Srv, p2Srv}, []*httptest.Server{d1Srv}, nil)
	store := session.NewStore(10*time.Minute, 1000)
	st.handler.SetSessionStore(store)

	do := func() int {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		req.Header.Set("X-Session-Id", "sess-2")
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("首请求应 200，实际 %d", code)
	}
	// 绑定已建立（指向首请求命中的 prefill）。
	if id, ok := store.Lookup("sess-2"); !ok || id == "" {
		t.Fatal("首请求后应有会话绑定")
	}
	// 熔断摘除被绑定的 prefill（哪个被绑就摘哪个）。
	boundID, _ := store.Lookup("sess-2")
	boundPre := st.reg.Get(boundID)
	if boundPre == nil || boundPre.ID != boundID {
		t.Fatalf("绑定指向的 prefill 应存在: %q", boundID)
	}
	boundPre.MarkFailure(1, time.Minute)
	if code := do(); code != http.StatusOK {
		t.Fatalf("摘除绑定 prefill 后应回退成功，实际 %d", code)
	}
	if id, ok := store.Lookup("sess-2"); !ok || id == boundPre.ID {
		t.Fatalf("失效绑定应自愈为未绑定或指向新 prefill，实际 id=%q ok=%v", id, ok)
	}
	// prefill 总请求数：p1+p2 各至少 1（回退后确实换了实例）。
	total := p1.ReqCount() + p2.ReqCount()
	if total < 2 {
		t.Fatalf("摘除后应触发换 prefill（累计 ≥2 次请求），实际 %d", total)
	}
}

// TestM16ResidualBigIntKvTransferParams 复现并翻转：prefill 响应的
// kv_transfer_params 含超 float64 精度的大整数时，注入 decode 请求不得失真
// （修复前 json.Unmarshal → float64 → 9007199254740993 变 9007199254740992）。
func TestM16ResidualBigIntKvTransferParams(t *testing.T) {
	var decodeBody []byte
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 大整数块 id：超过 float64 精确表示上限。
		io.WriteString(w, `{"kv_transfer_params":{"remote_block_ids":[9007199254740993,-9223372036854775807]},"ok":true}`)
	}))
	defer prefillSrv.Close()
	decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer decodeSrv.Close()

	st := newPDStack(t, "vllm", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	// decode 侧必须实际收到请求（prefill 响应解析成功、KV 句柄已注入）。
	if rec.Code != http.StatusOK {
		t.Fatalf("请求应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 修复前：remote_block_ids 经 float64 失真；修复后：原样字节保留。
	for _, want := range []string{"9007199254740993", "-9223372036854775807"} {
		if !strings.Contains(string(decodeBody), want) {
			t.Fatalf("decode 请求应原样携带大整数 %s，实际: %s", want, decodeBody)
		}
	}
}
