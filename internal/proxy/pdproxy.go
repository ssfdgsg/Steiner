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
	"ai-gateway/internal/scheduler"
)

// sideError 标记 PD 转发失败发生在哪一侧，servePD 据此只排除故障侧实例重试，
// 避免 decode 侧抖动误伤正常的 prefill（反之亦然）。
type sideError struct {
	side string // "prefill" | "decode"
	err  error
}

func (e *sideError) Error() string { return e.err.Error() }
func (e *sideError) Unwrap() error { return e.err }

// pdFail 构造带侧别标记的 PD 转发错误。
func pdFail(side, format string, args ...interface{}) error {
	return &sideError{side: side, err: fmt.Errorf(format, args...)}
}

// servePD PD 分离入口：选出 (prefill, decode, link)，按引擎家族转发；
// 失败按侧别分别排除后重试。
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

	excludePrefill := map[string]bool{}
	excludeDecode := map[string]bool{}
	attempts := h.cfg.Server.Retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		prefill, decode, link, err := group.Pick(h.sched, req, excludePrefill, excludeDecode)
		if err != nil {
			h.gw.PickErrors.WithLabelValues(route.Name, "pd_no_pair").Inc()
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		done, err := h.tryPDPair(w, r, route, req, doc, prefill, decode, link)
		if done {
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
	writeError(w, http.StatusBadGateway, msg)
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

	var done, handedOff bool
	var err error
	switch prefill.Engine.Family() {
	case "sglang":
		done, handedOff, err = h.pdSGLang(w, r, route, req, doc, prefill, decode)
	default:
		done, err = h.pdVLLM(w, r, route, req, doc, prefill, decode)
	}
	if !handedOff {
		prefill.Release()
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
		prefill.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, pdFail("prefill", "prefill %s 请求失败: %v", prefill.ID, err)
	}
	prefillRaw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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

	// 提取 KV 传输句柄。
	var prefillResp map[string]interface{}
	ktp := interface{}(nil)
	if json.Unmarshal(prefillRaw, &prefillResp) == nil {
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
// 返回 handedOff=true 表示 prefill 额度所有权已移交后台 goroutine 释放。
func (h *Handler) pdSGLang(w http.ResponseWriter, r *http.Request, route *backend.Route, req *scheduler.Request, doc map[string]interface{}, prefill, decode *backend.Backend) (done bool, handedOff bool, err error) {
	start := time.Now()
	room := rand.Int63()

	injected := cloneDoc(doc)
	injected["bootstrap_host"] = prefill.URL.Hostname()
	injected["bootstrap_port"] = prefill.BootstrapPort
	injected["bootstrap_room"] = room
	// prefill 与 decode 携带完全相同的载荷：序列化一次，两处复用。
	body, err := json.Marshal(injected)
	if err != nil {
		return false, false, pdFail("prefill", "序列化 PD 请求失败: %v", err)
	}

	// prefill 侧后台发出：其响应不回传客户端，只校验错误与回收额度。
	// 使用独立 context（仅继承 span 亲缘），避免客户端断开影响 KV 会合；
	// 经 doUpstream 转发，携带客户端原始请求头（鉴权、X-Request-Id）与 traceparent。
	_, pspan := tracer().Start(r.Context(), "gateway.pd.prefill",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gateway.backend", prefill.ID),
			attribute.Int64("gateway.pd.room", room),
		))
	pctx, pcancel := context.WithTimeout(trace.ContextWithSpan(context.Background(), pspan),
		h.cfg.Server.UpstreamResponseTimeout.D())
	go func() {
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
	return done, true, err
}

// forwardDecode decode 侧转发与流式回传（vllm/sglang 家族共用尾段）。
func (h *Handler) forwardDecode(w http.ResponseWriter, r *http.Request, route *backend.Route, decode *backend.Backend, body []byte, start time.Time, extraAttrs ...attribute.KeyValue) (bool, error) {
	attrs := append([]attribute.KeyValue{attribute.String("gateway.backend", decode.ID)}, extraAttrs...)
	dctx, dspan := tracer().Start(r.Context(), "gateway.pd.decode",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))
	defer dspan.End()
	dresp, err := h.doUpstream(dctx, r, decode, r.URL.Path, r.URL.RawQuery, body)
	if err != nil {
		dspan.RecordError(err)
		dspan.SetStatus(otelcodes.Error, "connect")
		decode.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, pdFail("decode", "decode %s 请求失败: %v", decode.ID, err)
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
