// PD 分离转发：按引擎家族选择两种协议。
//
// vllm 家族（NIXL/NCCL KVConnector 两段式，参考 vllm disaggregated serving 示例代理）：
//  1. 向 prefill 发送改写请求：max_tokens=1、关闭流式、携带 kv_transfer_params
//     （do_remote_decode=true），prefill 完成后在响应中返回 KV 块句柄；
//  2. 把 prefill 响应中的 kv_transfer_params 注入原请求发给 decode，
//     decode 经传输通道拉取 KV 后继续生成，响应流式回传客户端。
//
// sglang 家族（bootstrap 三元组并发双发，参考 sglang-router PD 模式）：
//
//	同一请求并发发给 prefill 与 decode，双方携带相同的
//	bootstrap_host / bootstrap_port / bootstrap_room 完成会合；
//	客户端响应取自 decode 流，prefill 响应仅校验错误。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/scheduler"
)

// sideError 标记 PD 转发失败发生在哪一侧，servePD 据此只排除故障侧实例重试，
// 避免 decode 侧抖动误伤正常的 prefill（反之亦然）。
type sideError struct {
	side string // "prefill" | "decode"
	code int    // 上游 HTTP 状态码（decode >=500 时透传用）；0 = 非状态码失败
	err  error
}

func (e *sideError) Error() string { return e.err.Error() }
func (e *sideError) Unwrap() error { return e.err }

// pdFail 构造带侧别标记的 PD 转发错误。
func pdFail(side, format string, args ...interface{}) error {
	return &sideError{side: side, err: fmt.Errorf(format, args...)}
}

// pdStatusFail 构造带侧别与上游状态码的 PD 转发错误（decode 返回 >=500 时，
// 供 servePD 在全部 decode 失败后透传最后一个状态码）。
func pdStatusFail(side string, code int, format string, args ...interface{}) error {
	return &sideError{side: side, code: code, err: fmt.Errorf(format, args...)}
}

// pdPrefillJoinTimeout 取消孤儿 prefill 后等待其 goroutine 退出的上限。
// ctx 取消下 doUpstream 会随传输层立即返回（毫秒级），此上限仅为防御握手
// 悬挂等异常；超时后放行并打 Warn，不阻塞重试。
const pdPrefillJoinTimeout = 2 * time.Second

// servePD PD 分离入口：选出 (prefill, decode, link)，按引擎家族转发；
// 失败按侧别分别排除后重试。H9：PD 路径补齐常规路径的容量排队与会话粘性
// （绑定 prefill 侧，KV 亲和）。
func (h *Handler) servePD(w http.ResponseWriter, r *http.Request, route *backend.Route, req *scheduler.Request, doc map[string]interface{}) {
	if doc == nil {
		writeError(w, http.StatusBadRequest, "PD 分离模式要求 JSON 请求体")
		return
	}
	group := h.pdMgr.Get(route.PDGroup)
	if group == nil {
		writeError(w, http.StatusInternalServerError, "PD 组 "+route.PDGroup+" 未初始化")
		return
	}

	// H9 会话粘性：绑定的是 prefill 实例（KV 驻留侧）。绑定失效（已摘除/不可用）
	// 即时解除，与 serveNormal 的自愈语义一致；有效绑定作为选路偏好传入。
	preferPrefill := ""
	if h.sess != nil && req.SessionID != "" {
		if id, ok := h.sess.Lookup(req.SessionID); ok {
			if group.PrefillAvailable(id) {
				preferPrefill = id
			} else {
				h.sess.Unbind(req.SessionID)
			}
		}
	}

	excludePrefill := map[string]bool{}
	excludeDecode := map[string]bool{}
	attempts := h.cfg.Server.Retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		prefill, decode, link, err := h.pickPDWithQueue(r.Context(), group, route, req, preferPrefill, excludePrefill, excludeDecode)
		if err != nil {
			// 容量准入/排队类错误走 rejectPick 映射（429/503），其余按 PD 无对处理。
			if errors.Is(err, errAdmissionRejected) || errors.Is(err, queue.ErrFull) || errors.Is(err, queue.ErrTimeout) {
				h.rejectPick(w, route.Name, err)
				return
			}
			h.gw.PickErrors.WithLabelValues(route.Name, "pd_no_pair").Inc()
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		done, err := h.tryPDPair(w, r, route, req, doc, prefill, decode, link)
		if done {
			// H9：转发成功即绑定会话 → prefill（KV 亲和；失败/已处理错误不绑定）。
			if err == nil && h.sess != nil && req.SessionID != "" {
				h.sess.Bind(req.SessionID, prefill.ID)
			}
			return
		}
		// 只排除故障侧：无侧别信息时按 prefill 处理（保守，等价旧行为）。
		var se *sideError
		if errors.As(err, &se) && se.side == "decode" {
			excludeDecode[decode.ID] = true
		} else {
			excludePrefill[prefill.ID] = true
		}
		lastErr = err
		if i < attempts-1 {
			h.gw.Retries.Inc()
			slog.Warn("PD 转发失败，排除故障侧后重试",
				"group", group.Name, "prefill", prefill.ID, "decode", decode.ID, "err", err)
		}
	}
	h.gw.PickErrors.WithLabelValues(route.Name, "pd_exhausted").Inc()
	msg := "PD 分离转发失败"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	// H3：全部 decode 失败（均返回 >=500）时透传最后一个 decode 状态码
	// （通常是 503），与常规路径"耗尽后按上游错误码应答"语义一致；
	// prefill 侧失败或非状态码失败保持 502。
	code := http.StatusBadGateway
	var se *sideError
	if errors.As(lastErr, &se) && se.side == "decode" && se.code >= http.StatusInternalServerError {
		code = se.code
	}
	writeError(w, code, msg)
}

// tryPDPair 对一对 (prefill, decode) 执行一次完整 PD 转发。
// prefill 并发额度的所有权规则：仅当 sglang 后台 goroutine 确已启动（handedOff）
// 时由其释放，其余一切路径（vllm、sglang 启动前早退、未来新增分支）在此统一兜底，
// 不依赖各转发函数自行释放。
func (h *Handler) tryPDPair(w http.ResponseWriter, r *http.Request, route *backend.Route, req *scheduler.Request, doc map[string]interface{}, prefill, decode *backend.Backend, link *pd.Link) (bool, error) {
	if !prefill.TryAcquire() {
		return false, pdFail("prefill", "prefill %s 并发额度已满", prefill.ID)
	}
	if !decode.TryAcquire() {
		prefill.Release()
		return false, pdFail("decode", "decode %s 并发额度已满", decode.ID)
	}
	link.Acquire()
	defer h.signalCapacity()
	defer link.Release()
	defer decode.Release()

	// H4：sglang prefill 后台请求绑定"本次尝试"生命周期。尝试一结束（成功、
	// 换对重试、客户端断开、额度耗尽）即取消 prefill ctx，并在返回前等待其
	// goroutine 退出——重试路径上保证旧请求已终止后才重新选路/再发，杜绝
	// 孤儿 prefill 并发双发；客户端断开随 r.Context() 一并传播取消。
	actx, acancel := context.WithCancel(r.Context())
	defer acancel()

	var done, handedOff bool
	var err error
	var pdone <-chan struct{}
	switch prefill.Engine.Family() {
	case "sglang":
		done, handedOff, err, pdone = h.pdSGLang(actx, w, r, route, req, doc, prefill, decode)
	default:
		done, err = h.pdVLLM(w, r, route, req, doc, prefill, decode)
	}
	if !handedOff {
		prefill.Release()
	}
	if pdone != nil {
		// 尝试结束：取消孤儿 prefill 并等待其退出（限时），确保重试不会对同一
		// prefill 双发，也避免在 UpstreamResponseTimeout 窗口内占用并发额度。
		acancel()
		select {
		case <-pdone:
		case <-time.After(pdPrefillJoinTimeout):
			slog.Warn("PD prefill goroutine 未在限期内退出",
				"prefill", prefill.ID, "timeout", pdPrefillJoinTimeout.String())
		}
	}
	return done, err
}

// pdVLLM vllm 家族两段式转发。
func (h *Handler) pdVLLM(w http.ResponseWriter, r *http.Request, route *backend.Route, req *scheduler.Request, doc map[string]interface{}, prefill, decode *backend.Backend) (bool, error) {
	start := time.Now()

	// 第一段：prefill 请求。max_tokens=1 只做提示词计算，不生成。
	prefillDoc := cloneDoc(doc)
	prefillDoc["max_tokens"] = 1
	if _, ok := doc["max_completion_tokens"]; ok {
		prefillDoc["max_completion_tokens"] = 1
	}
	prefillDoc["stream"] = false
	delete(prefillDoc, "stream_options")
	prefillDoc["kv_transfer_params"] = map[string]interface{}{
		"do_remote_decode":  true,
		"do_remote_prefill": false,
		"remote_engine_id":  nil,
		"remote_block_ids":  nil,
		"remote_host":       nil,
		"remote_port":       nil,
	}
	prefillBody, err := json.Marshal(prefillDoc)
	if err != nil {
		return false, pdFail("prefill", "序列化 prefill 请求失败: %v", err)
	}

	pctx, pspan := tracer().Start(r.Context(), "gateway.pd.prefill",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("gateway.backend", prefill.ID)))
	resp, err := h.doUpstream(pctx, r, prefill, r.URL.Path, r.URL.RawQuery, prefillBody)
	if err != nil {
		pspan.RecordError(err)
		pspan.SetStatus(otelcodes.Error, "connect")
		pspan.End()
		// H4（H12 语义）：上游错误源于 ctx 取消（客户端已断开）——prefill 无辜，
		// 不记账、不换对重试（重试只会继续在已取消的 ctx 上失败），直接中止。
		if errors.Is(err, context.Canceled) {
			slog.Info("PD prefill 客户端断开，中止转发（不记账、不重试）", "prefill", prefill.ID, "err", err)
			writeError(w, http.StatusServiceUnavailable, "客户端已断开，转发中止")
			return true, err
		}
		prefill.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, pdFail("prefill", "prefill %s 请求失败: %v", prefill.ID, err)
	}
	// M12：LimitReader 截断不报错，kv_transfer_params 可能落在被丢弃的尾部。
	// 读完 4MiB 限制后 peek 1 字节，非 EOF 即存在截断，打 Warn 告警；
	// 行为不变：请求继续按截断后的数据（无 KV 句柄）处理。
	prefillRaw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err == nil {
		var probe [1]byte
		if n, perr := resp.Body.Read(probe[:]); n > 0 || (perr != nil && !errors.Is(perr, io.EOF)) {
			slog.Warn("prefill 响应超过 4MiB 被截断,可能丢失 kv_transfer_params",
				"prefill", prefill.ID, "status", resp.StatusCode)
		}
	}
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		pspan.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		pspan.SetStatus(otelcodes.Error, "bad_status")
		pspan.End()
		prefill.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, pdFail("prefill", "prefill %s 返回 %d", prefill.ID, resp.StatusCode)
	}
	pspan.End()
	prefill.MarkSuccess()
	h.sched.Observe(req, prefill.ID) // 提示词 KV 驻留在 prefill 侧

	// 提取 KV 传输句柄。M16 残余修复：prefill 响应用 json.Decoder + UseNumber
	// 解析——kv_transfer_params 中的大整数（如远端块 id）不再经 float64 失真，
	// 注入 decode 请求重序列化时原样保留。
	var prefillResp map[string]interface{}
	ktp := interface{}(nil)
	dec := json.NewDecoder(bytes.NewReader(prefillRaw))
	dec.UseNumber()
	if dec.Decode(&prefillResp) == nil {
		// 拒绝尾随非空白内容（与 json.Unmarshal 语义一致）。
		if _, err := dec.Token(); err != io.EOF {
			prefillResp = nil
		}
	}
	if prefillResp != nil {
		ktp = prefillResp["kv_transfer_params"]
	}

	// 第二段：decode 请求，注入 kv_transfer_params。
	decodeDoc := cloneDoc(doc)
	if ktp != nil {
		decodeDoc["kv_transfer_params"] = ktp
	}
	decodeBody, err := json.Marshal(decodeDoc)
	if err != nil {
		return false, pdFail("decode", "序列化 decode 请求失败: %v", err)
	}
	return h.forwardDecode(w, r, route, decode, decodeBody, start)
}

// pdSGLang sglang 家族 bootstrap 并发双发。
// parent 为本次尝试的 ctx（tryPDPair 以客户端请求 ctx 派生）：prefill 后台请求
// 绑定尝试生命周期，尝试结束或客户端断开都会取消它（H4）。
// 返回 handedOff=true 表示 prefill 额度所有权已移交后台 goroutine 释放；
// 返回 pdone 通道，prefill goroutine 退出（含释放额度）后关闭，
// 供 tryPDPair 在尝试结束时取消并等待旧请求终止，避免孤儿双发。
func (h *Handler) pdSGLang(parent context.Context, w http.ResponseWriter, r *http.Request, route *backend.Route, req *scheduler.Request, doc map[string]interface{}, prefill, decode *backend.Backend) (done bool, handedOff bool, err error, pdone <-chan struct{}) {
	start := time.Now()
	room := rand.Int63()

	injected := cloneDoc(doc)
	injected["bootstrap_host"] = prefill.URL.Hostname()
	injected["bootstrap_port"] = prefill.BootstrapPort
	injected["bootstrap_room"] = room
	// prefill 与 decode 携带完全相同的载荷：序列化一次，两处复用。
	body, err := json.Marshal(injected)
	if err != nil {
		return false, false, pdFail("prefill", "序列化 PD 请求失败: %v", err), nil
	}

	// prefill 侧后台发出：其响应不回传客户端，只校验错误与回收额度。
	// H4：ctx 绑定本次尝试（parent）并叠加 UpstreamResponseTimeout 预算——KV
	// 会合只在链路存活时才有意义，尝试结束（成功/换对/超时）或客户端断开即
	// 取消旧 prefill，杜绝孤儿双发；预算内未应答（DeadlineExceeded）仍视为
	// 真实故障记账。经 doUpstream 转发，携带客户端原始请求头与 traceparent。
	pctx, pspan := tracer().Start(parent, "gateway.pd.prefill",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gateway.backend", prefill.ID),
			attribute.Int64("gateway.pd.room", room),
		))
	pctx, pcancel := context.WithTimeout(pctx, h.cfg.Server.UpstreamResponseTimeout.D())
	doneCh := make(chan struct{})
	go func() {
		// defer LIFO：先释放额度并收尾 span，最后关闭 pdone 通知等待方。
		defer close(doneCh)
		defer pcancel()
		defer pspan.End()
		defer func() {
			prefill.Release()
			h.signalCapacity()
		}()
		presp, err := h.doUpstream(pctx, r, prefill, r.URL.Path, r.URL.RawQuery, body)
		if err != nil {
			pspan.RecordError(err)
			pspan.SetStatus(otelcodes.Error, "connect")
			// H4/H12：ctx 取消（客户端断开，或本次尝试结束主动取消）不记失败——
			// prefill 无辜，记账会把健康 prefill 误熔断；真实连接失败与预算超时
			//（DeadlineExceeded）仍按失败记账。
			if errors.Is(err, context.Canceled) {
				slog.Info("PD prefill 已取消（尝试结束或客户端断开），不记账", "prefill", prefill.ID, "room", room, "err", err)
				return
			}
			prefill.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
			slog.Warn("PD prefill 侧请求失败", "prefill", prefill.ID, "room", room, "err", err)
			return
		}
		defer presp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(presp.Body, 4<<20))
		if presp.StatusCode >= 400 {
			pspan.SetAttributes(attribute.Int("http.response.status_code", presp.StatusCode))
			pspan.SetStatus(otelcodes.Error, "bad_status")
			prefill.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
			slog.Warn("PD prefill 侧返回错误", "prefill", prefill.ID, "room", room, "code", presp.StatusCode)
			return
		}
		prefill.MarkSuccess()
		// prefill 确认成功后才登记前缀归属，与 vllm 路径语义一致，
		// 避免失败的 prefill 在 TTL 内误导 cache_aware/prefix_match 亲和。
		h.sched.Observe(req, prefill.ID)
	}()

	// decode 侧走主路径，响应流式回传客户端。
	done, err = h.forwardDecode(w, r, route, decode, body, start,
		attribute.Int64("gateway.pd.room", room))
	return done, true, err, doneCh
}

// forwardDecode decode 侧转发与流式回传（vllm/sglang 家族共用尾段）。
// 返回 done=false 表示未污染下游连接（连接失败或 decode 返回 >=500），
// 供 servePD 排除该 decode 后重试；与常规转发路径的换后端重试语义一致。
func (h *Handler) forwardDecode(w http.ResponseWriter, r *http.Request, route *backend.Route, decode *backend.Backend, body []byte, start time.Time, extraAttrs ...attribute.KeyValue) (bool, error) {
	attrs := append([]attribute.KeyValue{attribute.String("gateway.backend", decode.ID)}, extraAttrs...)
	dctx, dspan := tracer().Start(r.Context(), "gateway.pd.decode",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))
	defer dspan.End()
	dresp, err := h.doUpstream(dctx, r, decode, r.URL.Path, r.URL.RawQuery, body)
	if err != nil {
		// H4/H12：上游错误源于 ctx 取消（客户端已断开）——decode 无辜，不记账、
		// 不换 decode 重试（在已取消的 ctx 上重试只会继续失败，制造重试风暴与
		// 误熔断），直接中止并向客户端返回。
		if errors.Is(err, context.Canceled) {
			dspan.RecordError(err)
			dspan.SetStatus(otelcodes.Error, "client_disconnect")
			slog.Info("PD decode 客户端断开，中止转发（不记账、不重试）", "decode", decode.ID, "err", err)
			writeError(w, http.StatusServiceUnavailable, "客户端已断开，转发中止")
			return true, err
		}
		dspan.RecordError(err)
		dspan.SetStatus(otelcodes.Error, "connect")
		decode.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, pdFail("decode", "decode %s 请求失败: %v", decode.ID, err)
	}
	// H3：decode 返回网关类错误码（>=500）时，尚未写出任何字节，视为可重试失败：
	// MarkFailure（达到阈值后熔断摘除，Pick 的 Available 检查生效）并让外层
	// 排除该 decode 重试；若全部 decode 都失败，servePD 透传最后一个状态码。
	if dresp.StatusCode >= http.StatusInternalServerError {
		io.Copy(io.Discard, io.LimitReader(dresp.Body, 4096))
		dresp.Body.Close()
		dspan.SetAttributes(attribute.Int("http.response.status_code", dresp.StatusCode))
		dspan.SetStatus(otelcodes.Error, "bad_status")
		h.gw.UpstreamErrors.WithLabelValues(decode.ID, "bad_status").Inc()
		decode.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, pdStatusFail("decode", dresp.StatusCode, "decode %s 返回 %d", decode.ID, dresp.StatusCode)
	}
	decode.MarkSuccess()

	code := h.streamResponse(dctx, w, dresp, decode.ID, route.Name, start)
	dspan.SetAttributes(attribute.Int("http.response.status_code", code))
	h.gw.ReqTotal.WithLabelValues(decode.ID, route.Name, strconv.Itoa(code)).Inc()
	h.gw.ReqDuration.WithLabelValues(decode.ID, route.Name).Observe(time.Since(start).Seconds())
	h.observeResult(decode.ID, code)
	return true, nil
}

// cloneDoc 浅拷贝 JSON 文档（顶层字段改写不影响原文档）。
func cloneDoc(doc map[string]interface{}) map[string]interface{} {
	nd := make(map[string]interface{}, len(doc)+4)
	for k, v := range doc {
		nd[k] = v
	}
	return nd
}

// pickPDWithQueue PD 选对，镜像常规路径 pickWithQueue 的容量语义（H9）：
// 选对失败且启用了排队时，先过容量准入再挂起等待容量（同模型配额），
// 等待期间轮询选对；未启用排队/准入时保持原语义（失败即返回）。
func (h *Handler) pickPDWithQueue(ctx context.Context, group *pd.Group, route *backend.Route, req *scheduler.Request, preferPrefill string, excludePrefill, excludeDecode map[string]bool) (*backend.Backend, *backend.Backend, *pd.Link, error) {
	prefill, decode, link, err := group.PickPreferred(h.sched, req, preferPrefill, excludePrefill, excludeDecode)
	if err == nil || h.q == nil {
		return prefill, decode, link, err
	}
	if !h.admitToQueue(route, req) {
		return nil, nil, nil, errAdmissionRejected
	}
	_, qspan := tracer().Start(ctx, "gateway.queue_wait",
		trace.WithAttributes(attribute.String("gateway.model", route.Name)))
	defer qspan.End()
	qerr := h.q.WaitFor(ctx, route.Name, func() bool {
		if p2, d2, l2, e2 := group.PickPreferred(h.sched, req, preferPrefill, excludePrefill, excludeDecode); e2 == nil {
			prefill, decode, link = p2, d2, l2
			return true
		}
		return false
	})
	if qerr != nil {
		qspan.RecordError(qerr)
		qspan.SetStatus(otelcodes.Error, qerr.Error())
		return nil, nil, nil, qerr
	}
	return prefill, decode, link, nil
}
