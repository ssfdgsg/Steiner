// Package proxy 实现 OpenAI 兼容 API 的反向代理：
//   - 请求级调度（接入 scheduler 的全部策略）；
//   - SSE 流式透传（逐块刷出，TTFT 打点）;
//   - 未写出首字节前的自动换后端重试与被动摘除；
//   - 模型级令牌桶限流；
//   - PD 分离两段式转发（见 pdproxy.go）。
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/expr-lang/expr/vm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"ai-gateway/internal/alerting"
	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/scheduler"
	"ai-gateway/pkg/openai"
)

// Limiter 限流器抽象：进程内令牌桶（单机）或集群级共享配额（internal/cluster）。
type Limiter interface {
	Allow(ctx context.Context) bool
}

// localLimiter 进程内令牌桶对 Limiter 的适配。
type localLimiter struct{ *rate.Limiter }

func (l localLimiter) Allow(context.Context) bool { return l.Limiter.Allow() }

// SessionStore 会话粘性存储抽象：本地绑定表（session.Store）或
// 集群共享绑定表（internal/cluster）。
type SessionStore interface {
	Lookup(key string) (string, bool)
	Bind(key, backendID string)
	// Unbind 解除单个会话的绑定（绑定已失效时清理，本地+共享同步）。
	Unbind(key string)
	// InvalidateBackend 后端下线/摘除时批量清除其全部会话绑定。
	InvalidateBackend(backendID string)
}

// Handler 代理处理器。
type Handler struct {
	cfg      *config.Config
	reg      *backend.Registry
	sched    *scheduler.Scheduler
	pdMgr    *pd.Manager
	gw       *metrics.Gateway
	client   *http.Client
	limiters map[string]Limiter
	models   []string

	// sess 会话粘性存储，可为 nil（未启用）。
	sess SessionStore
	// q 容量排队队列，可为 nil（未启用）。
	q *queue.Queue
	// admission 排队容量准入表达式，可为 nil（不启用）。
	admission    *vm.Program
	admissionSrc string
	// observer 转发结果/TTFT 观测回调（金丝雀发布判据数据源），可为 nil。
	observer ResultObserver
}

// ResultObserver 接收每次转发的结果与首字节时延（internal/rollout 实现）。
type ResultObserver interface {
	ObserveResult(backendID string, code int)
	ObserveTTFT(backendID string, ttftSeconds float64)
}

// SetResultObserver 注入转发观测回调（装配期调用）。
func (h *Handler) SetResultObserver(o ResultObserver) { h.observer = o }

// observeResult / observeTTFT nil 安全的上报入口。
func (h *Handler) observeResult(backendID string, code int) {
	if h.observer != nil {
		h.observer.ObserveResult(backendID, code)
	}
}

func (h *Handler) observeTTFT(backendID string, sec float64) {
	if h.observer != nil {
		h.observer.ObserveTTFT(backendID, sec)
	}
}

// SetSessionStore 启用会话粘性（装配期调用）。
func (h *Handler) SetSessionStore(s SessionStore) { h.sess = s }

// SetLimiter 覆盖某一模型的限流器（装配期调用，集群模式注入分布式限流）。
func (h *Handler) SetLimiter(model string, l Limiter) { h.limiters[model] = l }

// SetQueue 启用容量排队（装配期调用）。
func (h *Handler) SetQueue(q *queue.Queue) { h.q = q }

// SetAdmission 启用排队容量准入（装配期调用）：请求将要排队时求值该布尔
// 表达式，false 立即 429，避免明显装不下的请求排到超时。
func (h *Handler) SetAdmission(prog *vm.Program, src string) {
	h.admission = prog
	h.admissionSrc = src
}

// signalCapacity 通知排队中的请求：容量可能已释放。
func (h *Handler) signalCapacity() {
	if h.q != nil {
		h.q.Signal()
	}
}

// QueueStats 排队观测视图（admin 用）：是否启用、全局深度与按模型深度。
func (h *Handler) QueueStats() (enabled bool, total int64, byModel map[string]int64) {
	if h.q == nil {
		return false, 0, nil
	}
	return true, h.q.Depth(), h.q.Depths()
}

// New 构造代理处理器。
func New(cfg *config.Config, reg *backend.Registry, sched *scheduler.Scheduler, pdMgr *pd.Manager, gw *metrics.Gateway) *Handler {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.Server.UpstreamConnectTimeout.D(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: cfg.Server.UpstreamResponseTimeout.D(),
		// 流式响应必须立即透传，禁用透明压缩以免破坏 SSE 分帧。
		DisableCompression: true,
	}
	h := &Handler{
		cfg:      cfg,
		reg:      reg,
		sched:    sched,
		pdMgr:    pdMgr,
		gw:       gw,
		client:   &http.Client{Transport: transport}, // 不设整体超时：长流式响应合法
		limiters: map[string]Limiter{},
	}
	for _, m := range cfg.Models {
		if m.Name != "*" {
			h.models = append(h.models, m.Name)
		}
		if m.RateLimitQPS > 0 {
			h.limiters[m.Name] = localLimiter{rate.NewLimiter(rate.Limit(m.RateLimitQPS), m.RateLimitBurst)}
		}
	}
	return h
}

// ServeHTTP 入口：/v1/models 本地应答，其余 /v1/* 转发。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		h.serveModels(w)
		return
	}

	// 追踪根 span：续接客户端 traceparent；请求 ID 与 trace ID 回写响应头，
	// 三者互相关联（日志查 request_id，追踪查 trace_id）。
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newRequestID()
		r.Header.Set("X-Request-Id", reqID)
	}
	ctx, span := tracer().Start(ctx, r.Method+" "+r.URL.Path,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
			attribute.String("gateway.request_id", reqID),
		))
	defer span.End()
	r = r.WithContext(ctx)
	w.Header().Set("X-Request-Id", reqID)
	if sc := span.SpanContext(); sc.HasTraceID() {
		w.Header().Set("X-Trace-Id", sc.TraceID().String())
	}
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		span.SetAttributes(attribute.Int("http.response.status_code", sw.Status()))
		if sw.Status() >= http.StatusInternalServerError {
			span.SetStatus(otelcodes.Error, http.StatusText(sw.Status()))
		}
	}()
	w = sw

	var body []byte
	if r.Body != nil {
		var err error
		var tooLarge bool
		body, tooLarge, err = readRequestBody(r, h.cfg.Server.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		if tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("请求体超过上限 %d 字节", h.cfg.Server.MaxBodyBytes))
			return
		}
	}

	// 先只扫描 model/stream/user/priority 等小字段。对无需 prompt 特征的普通路由，
	// messages/prompt 大字段不会被解码成第二份字符串或 map 树。
	req := parseRequestMeta(r, body)
	route, err := h.reg.Route(req.Model)
	if err != nil {
		h.gw.PickErrors.WithLabelValues(req.Model, "no_route").Inc()
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	span.SetAttributes(
		attribute.String("gateway.model", req.Model),
		attribute.String("gateway.route", route.Name),
		attribute.Bool("gateway.stream", req.Stream),
	)

	if lim, ok := h.limiters[route.Name]; ok && !lim.Allow(r.Context()) {
		h.gw.RateLimited.WithLabelValues(route.Name).Inc()
		span.AddEvent("rate_limited")
		writeError(w, http.StatusTooManyRequests, "模型 "+route.Name+" 触发限流")
		return
	}

	// 仅在调度策略、KV 前缀树、容量准入、PD 或模型改写真正需要时解码完整文档。
	// 无 KV 的负载类策略直接复用原始 body 转发，避免额外 prompt 大对象。
	needFeatures, promptLimit := h.sched.PromptRequirements(route, req.SessionID != "")
	if h.admission != nil {
		needFeatures = true
	}
	if route.PDGroup != "" {
		// PD 组策略不挂在 Route 上，保守保留既有完整请求特征语义。
		needFeatures = true
		promptLimit = -1
	}
	needDocument := needFeatures || route.RewriteModel != "" || route.PDGroup != ""
	var doc map[string]interface{}
	if needDocument {
		doc = parseRequestDocument(body)
		if doc != nil {
			populateRequestBase(req, doc)
			applySessionHeader(r, req)
			if needFeatures {
				populatePromptFeatures(req, doc, promptLimit)
			}
		}
	}

	// 模型名改写：对外统一模型名 -> 后端实际部署名。
	// doc 是 PD 路径的转发载体，body 是常规路径的转发载体，两者都要更新。
	if route.RewriteModel != "" && doc != nil {
		doc["model"] = route.RewriteModel
		if nb, err := json.Marshal(doc); err == nil {
			body = nb
		}
	}

	if route.PDGroup != "" {
		h.servePD(w, r, route, req, doc)
		return
	}
	h.serveNormal(w, r, route, req, body)
}

// readRequestBody 在已知 Content-Length 时一次精确分配，避免 LimitReader 隐藏长度后
// io.ReadAll 多轮扩容复制大请求；chunked/未知长度仍保留 max+1 的严格上限检查。
func readRequestBody(r *http.Request, maxBytes int64) (body []byte, tooLarge bool, err error) {
	if r.ContentLength > maxBytes {
		return nil, true, nil
	}
	if r.ContentLength >= 0 {
		maxInt := int64(^uint(0) >> 1)
		if r.ContentLength > maxInt {
			return nil, true, nil
		}
		body = make([]byte, int(r.ContentLength))
		if len(body) == 0 {
			return body, false, nil
		}
		_, err = io.ReadFull(r.Body, body)
		return body, false, err
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	return body, int64(len(body)) > maxBytes, nil
}

// serveNormal 常规池转发：会话粘性短路 -> 选路（容量不足可排队）->
// 转发 -> 失败则换后端重试。
func (h *Handler) serveNormal(w http.ResponseWriter, r *http.Request, route *backend.Route, req *scheduler.Request, body []byte) {
	exclude := map[string]bool{}
	attempts := h.cfg.Server.Retries + 1
	var lastErr error

	// 会话粘性：绑定的后端仍可用则首选；转发失败后回落正常选路并改绑。
	// L2：绑定指向已摘除（不在路由池）或不可用（健康摘除/冷却/隔离）的后端
	// 即视为失效——立即 Unbind 清理，避免死绑定被后续 Lookup 反复滑动续期、
	// 占满表容量淘汰活跃会话；清理后照常回落选路，路由正确性不受影响。
	var preferred *backend.Backend
	if h.sess != nil && req.SessionID != "" {
		if id, ok := h.sess.Lookup(req.SessionID); ok {
			if b := routeBackend(route, id); b != nil && b.Available(time.Now()) {
				preferred = b
			} else {
				h.sess.Unbind(req.SessionID)
			}
		}
	}

	for i := 0; i < attempts; i++ {
		var b *backend.Backend
		var err error
		if preferred != nil {
			b, preferred = preferred, nil
		} else {
			pickStart := time.Now()
			pickCtx, pickSpan := tracer().Start(r.Context(), "gateway.pick",
				trace.WithAttributes(
					attribute.String("gateway.strategy", route.Strategy),
					attribute.Int("gateway.attempt", i),
				))
			b, err = h.pickWithQueue(pickCtx, route, req, exclude)
			h.gw.PickDuration.WithLabelValues(route.Strategy).Observe(time.Since(pickStart).Seconds())
			if err != nil {
				pickSpan.RecordError(err)
				pickSpan.SetStatus(otelcodes.Error, err.Error())
				pickSpan.End()
				h.rejectPick(w, route.Name, err)
				return
			}
			pickSpan.SetAttributes(attribute.String("gateway.backend", b.ID))
			pickSpan.End()
		}
		if !b.TryAcquire() {
			// 满载是选路容量检查与 TryAcquire 之间的瞬时竞态，不加入 exclude：
			// 永久排除会让后续排队重试（复用同一 exclude）在容量释放后也选不回该后端。
			lastErr = fmt.Errorf("后端 %s 并发额度已满", b.ID)
			continue
		}
		done, err := h.tryForward(w, r, b, route.Name, req, body)
		b.Release()
		h.signalCapacity()
		if done {
			return
		}
		exclude[b.ID] = true
		lastErr = err
		if i < attempts-1 {
			h.gw.Retries.Inc()
			slog.Warn("转发失败，换后端重试", "backend", b.ID, "model", route.Name, "err", err)
		}
	}
	h.gw.PickErrors.WithLabelValues(route.Name, "exhausted").Inc()
	msg := "全部后端转发失败"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeError(w, http.StatusBadGateway, msg)
}

// routeBackend 在路由池中按 ID 查后端（会话粘性用），不存在返回 nil。
func routeBackend(route *backend.Route, id string) *backend.Backend {
	for _, b := range route.Pool() {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// errAdmissionRejected 容量准入拒绝：不排队，立即快速失败。
var errAdmissionRejected = errors.New("容量准入拒绝：按当前集群水位与请求成本估算，排队大概率超时")

// pickWithQueue 选路；无可用容量且启用了排队时，先过容量准入再挂起等待。
// 排队时长单独成 span，慢请求可直接看出是否卡在等容量。
func (h *Handler) pickWithQueue(ctx context.Context, route *backend.Route, req *scheduler.Request, exclude map[string]bool) (*backend.Backend, error) {
	b, err := h.sched.Pick(route, req, exclude)
	if err == nil || h.q == nil {
		return b, err
	}
	if !h.admitToQueue(route, req) {
		return nil, errAdmissionRejected
	}
	_, qspan := tracer().Start(ctx, "gateway.queue_wait",
		trace.WithAttributes(attribute.String("gateway.model", route.Name)))
	defer qspan.End()
	qerr := h.q.WaitFor(ctx, route.Name, func() bool {
		if b2, e2 := h.sched.Pick(route, req, exclude); e2 == nil {
			b = b2
			return true
		}
		return false
	})
	if qerr != nil {
		qspan.RecordError(qerr)
		qspan.SetStatus(otelcodes.Error, qerr.Error())
		return nil, qerr
	}
	return b, nil
}

// admitToQueue 求值容量准入表达式。环境 = 该模型路由的集群聚合 + 请求特征；
// 求值失败按放行处理（fail-open：准入是优化，不能因表达式异常拒绝正常请求）。
func (h *Handler) admitToQueue(route *backend.Route, req *scheduler.Request) bool {
	if h.admission == nil {
		return true
	}
	env := alerting.ClusterEnv(route.Name, route.Pool(), time.Now())
	env["stream"] = req.Stream
	env["prompt_len"] = float64(req.PromptLen)
	env["prompt_tokens_est"] = float64(req.PromptTokensEst)
	env["is_multimodal"] = req.IsMultimodal
	env["image_count"] = float64(req.ImageCount)
	env["audio_count"] = float64(req.AudioCount)
	env["video_count"] = float64(req.VideoCount)
	out, err := vm.Run(h.admission, env)
	if err != nil {
		slog.Warn("容量准入表达式求值失败，按放行处理", "expr", h.admissionSrc, "err", err)
		return true
	}
	pass, ok := out.(bool)
	return !ok || pass
}

// rejectPick 按选路失败原因返回相应状态码与指标。
func (h *Handler) rejectPick(w http.ResponseWriter, model string, err error) {
	switch {
	case errors.Is(err, errAdmissionRejected):
		h.gw.PickErrors.WithLabelValues(model, "admission_rejected").Inc()
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, queue.ErrFull):
		h.gw.PickErrors.WithLabelValues(model, "queue_full").Inc()
		writeError(w, http.StatusTooManyRequests, "排队队列已满，请稍后重试")
	case errors.Is(err, queue.ErrTimeout):
		h.gw.PickErrors.WithLabelValues(model, "queue_timeout").Inc()
		writeError(w, http.StatusServiceUnavailable, "排队等待容量超时")
	default:
		h.gw.PickErrors.WithLabelValues(model, "no_backend").Inc()
		writeError(w, http.StatusServiceUnavailable, err.Error())
	}
}

// tryForward 向单个后端转发一次。
// 返回 done=true 表示响应已写出（成功，或已无法重试的失败）；
// done=false 表示未污染下游连接，可安全换后端重试。
func (h *Handler) tryForward(w http.ResponseWriter, r *http.Request, b *backend.Backend, model string, req *scheduler.Request, body []byte) (bool, error) {
	start := time.Now()
	fctx, fspan := tracer().Start(r.Context(), "gateway.forward",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gateway.backend", b.ID),
			attribute.String("server.address", b.URL.Host),
		))
	defer fspan.End()

	// H2：非流式请求的响应体读取套"预算型"期限（预算 = UpstreamResponseTimeout
	// 剩余时间，下限 30s）。ctx 到期后 transport 关闭上游连接，打断挂起的 body
	// 读取；流式（SSE 长流）不受限，仍由客户端连接生命周期控制。
	if !req.Stream {
		bctx, cancel := context.WithTimeout(fctx,
			nonStreamBodyBudget(h.cfg.Server.UpstreamResponseTimeout.D(), time.Since(start)))
		defer cancel()
		fctx = bctx
	}
	resp, err := h.doUpstream(fctx, r, b, r.URL.Path, r.URL.RawQuery, body)
	if err != nil {
		// H12：上游错误若源于 ctx 取消（客户端已断开），后端无辜——不 MarkFailure、
		// 不换后端重试（避免重试风暴与误熔断），直接以 503 中止并向客户端返回，
		// Info 日志与真实连接失败区分；真实连接失败（connect/超时等）才记账重试。
		if errors.Is(err, context.Canceled) {
			fspan.RecordError(err)
			fspan.SetStatus(otelcodes.Error, "client_disconnect")
			slog.Info("客户端断开，中止转发（不记账、不重试）", "backend", b.ID, "model", model, "err", err)
			writeError(w, http.StatusServiceUnavailable, "客户端已断开，转发中止")
			h.gw.ReqTotal.WithLabelValues(b.ID, model, strconv.Itoa(http.StatusServiceUnavailable)).Inc()
			return true, err
		}
		h.gw.UpstreamErrors.WithLabelValues(b.ID, "connect").Inc()
		fspan.RecordError(err)
		fspan.SetStatus(otelcodes.Error, "connect")
		b.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, err
	}
	// 网关类错误码（502/503/504）与引擎过载（429）视为后端不可用，可换后端重试；
	// 其余 4xx 是请求本身的问题，原样透传不重试。
	if resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout ||
		resp.StatusCode == http.StatusTooManyRequests {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		h.gw.UpstreamErrors.WithLabelValues(b.ID, "bad_status").Inc()
		fspan.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		fspan.SetStatus(otelcodes.Error, "bad_status")
		b.MarkFailure(int32(h.cfg.Server.FailureThreshold), h.cfg.Server.EjectCooldown.D())
		return false, fmt.Errorf("后端 %s 返回 %d", b.ID, resp.StatusCode)
	}

	code := h.streamResponse(fctx, w, resp, b.ID, model, start)
	fspan.SetAttributes(attribute.Int("http.response.status_code", code))
	// M4 语义：只有 2xx/3xx（<400）算转发成功 → MarkSuccess + 前缀归属 + 会话绑定；
	// 400~499（非 429）是客户端错误，透传但不记账、不粘会话、不观测；
	// 429/5xx 走失败路径（429 已在流式前拦截重试，此处可达的是已写出首字节的情形）。
	if code < http.StatusBadRequest {
		b.MarkSuccess()
		h.sched.Observe(req, b.ID)
		// 会话粘性：成功应答后（重新）绑定会话到本后端。
		if h.sess != nil && req.SessionID != "" {
			h.sess.Bind(req.SessionID, b.ID)
		}
		h.observeResult(b.ID, code)
	} else if code >= http.StatusInternalServerError {
		h.observeResult(b.ID, code)
	}
	h.gw.ReqTotal.WithLabelValues(b.ID, model, strconv.Itoa(code)).Inc()
	h.gw.ReqDuration.WithLabelValues(b.ID, model).Observe(time.Since(start).Seconds())
	return true, nil
}

// doUpstream 构造并发出上游请求。ctx 携带当前 span：
// traceparent 注入上游请求头，后端引擎若启用 OTel 可续接同一条 trace。
func (h *Handler) doUpstream(ctx context.Context, r *http.Request, b *backend.Backend, path, rawQuery string, body []byte) (*http.Response, error) {
	u := *b.URL
	u.Path = path
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Set("X-Forwarded-For", clientIP(r))
	if req.Header.Get("X-Request-Id") == "" {
		req.Header.Set("X-Request-Id", newRequestID())
	}
	// 覆盖客户端透传的 traceparent，使上游续接网关侧 span。
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	req.ContentLength = int64(len(body))
	return h.client.Do(req)
}

// streamResponse 把上游响应流式写回客户端，逐块刷出并在首字节处打 TTFT 点
// （指标 + span 事件）；透传同时保留响应尾部，流结束后提取 usage 记录 token 用量。
// ctx 携带当前转发 span。返回上游状态码。
func (h *Handler) streamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, backendID, model string, start time.Time) int {
	defer resp.Body.Close()
	span := trace.SpanFromContext(ctx)
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Upstream-Backend", backendID)
	w.WriteHeader(resp.StatusCode)

	// H2：非流式响应（非 SSE）在响应头到达后套"预算型"读取——读取循环经
	// deadlineReader 在预算到期时以超时错误退出，不再无限挂起；
	// SSE 长流保持不受限（由客户端连接生命周期控制）。
	body := io.Reader(resp.Body)
	if !isSSEResponse(resp) {
		if dl, ok := ctx.Deadline(); ok {
			body = &deadlineReader{rc: resp.Body, deadline: dl}
		}
	}

	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	first := true
	var tail tailBuffer
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if first {
				ttft := time.Since(start).Seconds()
				h.gw.TTFT.WithLabelValues(backendID, model).Observe(ttft)
				span.AddEvent("first_byte", trace.WithAttributes(
					attribute.Float64("gateway.ttft_seconds", ttft)))
				// 回写后端 TTFT 滑动均值：latency_first 等策略的反馈闭环信号。
				if b := h.reg.Get(backendID); b != nil {
					b.ObserveTTFT(ttft)
				}
				// 金丝雀发布判据的数据源。
				h.observeTTFT(backendID, ttft)
				first = false
			}
			tail.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				span.AddEvent("client_write_failed")
				return resp.StatusCode
			}
			// 每块立即刷出：SSE 流式响应的关键。
			_ = rc.Flush()
		}
		if err != nil {
			timedOut := errors.Is(err, errBodyReadTimeout) || errors.Is(err, context.DeadlineExceeded)
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				h.gw.UpstreamErrors.WithLabelValues(backendID, "stream").Inc()
				if timedOut {
					span.AddEvent("body_read_timeout")
				}
				span.AddEvent("stream_interrupt")
				span.RecordError(err)
				slog.Debug("上游响应流中断", "backend", backendID, "err", err)
			}
			if prompt, completion, ok := h.recordUsage(backendID, model, resp.StatusCode, tail.buf); ok {
				span.SetAttributes(
					attribute.Float64("gateway.prompt_tokens", prompt),
					attribute.Float64("gateway.completion_tokens", completion),
				)
			}
			return resp.StatusCode
		}
	}
}

// recordUsage 从响应尾部提取 token 用量并记账；仅统计成功响应。
// 返回提取结果供转发 span 附加属性。
func (h *Handler) recordUsage(backendID, model string, code int, tail []byte) (float64, float64, bool) {
	if code >= http.StatusMultipleChoices {
		return 0, 0, false
	}
	prompt, completion, ok := parseUsageTail(tail)
	if ok {
		h.gw.PromptTokens.WithLabelValues(backendID, model).Add(prompt)
		h.gw.CompletionTokens.WithLabelValues(backendID, model).Add(completion)
	}
	return prompt, completion, ok
}

// serveModels 用配置中的模型列表本地应答 /v1/models。
func (h *Handler) serveModels(w http.ResponseWriter) {
	items := make([]openai.ModelItem, 0, len(h.models))
	for _, m := range h.models {
		items = append(items, openai.ModelItem{ID: m, Object: "model", OwnedBy: "ai-gateway"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(openai.ModelList{Object: "list", Data: items})
}

// hopHeaders RFC 7230 定义的逐跳首部，转发时必须剥离。
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeError 以 OpenAI 兼容的错误结构应答。
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(openai.ErrorResponse{
		Error: openai.ErrorBody{Message: msg, Type: "gateway_error", Code: code},
	})
}

// errBodyReadTimeout 非流式响应体读取超出预算期限。
var errBodyReadTimeout = errors.New("上游响应体读取超时(超出预算)")

// deadlineReader 预算型读取包装：每次 Read 先检查期限，到期直接返回
// errBodyReadTimeout；底层读取返回后若已跨过期限（且非正常 EOF），也翻译为
// 超时错误。真正的连接中断由请求 ctx 截止期驱动（transport 在到期时关闭上游
// 连接，打断挂起的 Read），本包装负责把"到期"变成调用方可识别的超时错误。
type deadlineReader struct {
	rc       io.Reader
	deadline time.Time
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if time.Now().After(d.deadline) {
		return 0, errBodyReadTimeout
	}
	n, err := d.rc.Read(p)
	if n > 0 {
		return n, err // 数据优先交付，超时从下一次 Read 开始生效
	}
	if errors.Is(err, io.EOF) {
		return 0, err // 正常结束，即使略超期限也不误报超时
	}
	if err == nil {
		return 0, nil
	}
	if time.Now().After(d.deadline) {
		return 0, errBodyReadTimeout
	}
	return n, err
}

// nonStreamBodyBudget 非流式响应体的读取预算：UpstreamResponseTimeout 的剩余时间，
// 下限 30s（防止过小的配置误杀正常较长的非流式生成）。
func nonStreamBodyBudget(timeout, elapsed time.Duration) time.Duration {
	b := timeout - elapsed
	if b < 30*time.Second {
		b = 30 * time.Second
	}
	return b
}

// isSSEResponse 判断上游响应是否为 SSE 流（长流不套读超时）。
func isSSEResponse(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}
