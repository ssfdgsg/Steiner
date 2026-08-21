// Package server 组装 HTTP 接入层：数据面（/v1/*）、指标端点（/metrics）、
// 存活探针（/healthz）与管理面（/admin/*）。
// 使用 Go 1.22 标准库方法路由，不引入第三方 web 框架。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"ai-gateway/internal/alerting"
	"ai-gateway/internal/backend"
	"ai-gateway/internal/cluster"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/rollout"
	"ai-gateway/internal/scheduler"
	"ai-gateway/internal/store"
	"ai-gateway/internal/webui"
)

// Server HTTP 服务。
type Server struct {
	cfg   *config.Config
	reg   *backend.Registry
	pol   *policy.Engine
	sched *scheduler.Scheduler
	tree  *kvcache.Tree
	pdMgr *pd.Manager
	gw    *metrics.Gateway
	px    *proxy.Handler

	// alertEng / scaler 可为 nil（对应能力未启用）。
	alertEng *alerting.Engine
	scaler   *alerting.Autoscaler
	// clu 集群管理器，可为 nil（单机部署）。
	clu *cluster.Manager
	// st 动态配置持久层，可为 nil（未启用）。
	st *store.Store
	// ro 金丝雀发布管理器，可为 nil（未配置 rollouts）。
	ro *rollout.Manager
	// hist 管理面时序缓冲，可为 nil（未启用）。
	hist *metrics.History

	// started 进程启动时刻，累计统计的口径起点。

	// presetExplicit 各策略槽位的显式预设名（配置声明 + 管理端 apply），
	// 仅用于视图展示"当前生效方案"（L3：不反查手写表达式）。
	presetExplicit sync.Map
	started        time.Time

	// sessStore 会话粘性存储（为 nil 时摘除钩子跳过）：后端摘除时同步清除
	// 其会话绑定（M14 管理面路径；数据面 ServeNormal 已自愈兜底）。
	sessStore proxy.SessionStore

	httpSrv *http.Server
}

// SetSessionStore 注入会话粘性存储（装配期调用，M14 摘除联动）。
func (s *Server) SetSessionStore(ss proxy.SessionStore) { s.sessStore = ss }

// SetAlerting 注入告警引擎与扩缩容建议器（装配期调用，可传 nil）。
func (s *Server) SetAlerting(eng *alerting.Engine, sc *alerting.Autoscaler) {
	s.alertEng = eng
	s.scaler = sc
}

// SetCluster 注入集群管理器（装配期调用，可传 nil）：
// 策略热更新与后端增删将广播到全部实例，并启用 GET /admin/cluster 成员视图。
func (s *Server) SetCluster(c *cluster.Manager) { s.clu = c }

// SetStore 注入动态配置持久层（装配期调用，可传 nil）：
// admin 的后端增删与策略热更新将落库，重启后自动恢复。
func (s *Server) SetStore(st *store.Store) { s.st = st }

// SetRollouts 注入金丝雀发布管理器（装配期调用，可传 nil）。
func (s *Server) SetRollouts(ro *rollout.Manager) { s.ro = ro }

// SetStats 注入管理面时序缓冲（装配期调用，可传 nil）：
// 启用后 GET /admin/stats/history 返回最近若干小时的吞吐/时延/容量采样。
func (s *Server) SetStats(h *metrics.History) { s.hist = h }

// New 构造服务并注册全部路由。
func New(cfg *config.Config, reg *backend.Registry, pol *policy.Engine, sched *scheduler.Scheduler,
	tree *kvcache.Tree, pdMgr *pd.Manager, gw *metrics.Gateway, px *proxy.Handler) *Server {

	s := &Server{cfg: cfg, reg: reg, pol: pol, sched: sched, tree: tree, pdMgr: pdMgr, gw: gw, px: px,
		started: time.Now()}
	// 显式预设声明（L3）：仅配置声明或 /admin/presets/*/apply 产生的预设视为"当前方案"；
	// 手写表达式即使与预设逐字相同也不反查，杜绝误标。运行时热更不更新此表（文档化取舍）。
	for name, pc := range cfg.Policies {
		if pc.Preset != "" {
			s.presetExplicit.Store(name, pc.Preset)
		}
	}

	mux := http.NewServeMux()
	// 数据面：全部 /v1/* 交给代理。
	mux.Handle("/v1/", px)
	// 自身指标与探活。
	mux.Handle("GET /metrics", promhttp.HandlerFor(gw.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// 管理面。
	mux.HandleFunc("GET /admin/backends", s.handleBackends)
	mux.HandleFunc("POST /admin/backends", s.handleBackendPost)
	mux.HandleFunc("DELETE /admin/backends/{id}", s.handleBackendDelete)
	mux.HandleFunc("POST /admin/backends/{id}/cordon", s.handleCordon(true))
	mux.HandleFunc("POST /admin/backends/{id}/uncordon", s.handleCordon(false))
	mux.HandleFunc("GET /admin/policies", s.handlePoliciesGet)
	mux.HandleFunc("PUT /admin/policies/{name}", s.handlePolicyPut)
	mux.HandleFunc("POST /admin/policies/validate", s.handlePolicyValidate)
	mux.HandleFunc("GET /admin/presets", s.handlePresetsGet)
	mux.HandleFunc("POST /admin/presets/{name}/apply", s.handlePresetApply)
	mux.HandleFunc("GET /admin/explain", s.handleExplain)
	mux.HandleFunc("GET /admin/kvcache", s.handleKVCache)
	mux.HandleFunc("GET /admin/pd", s.handlePD)
	mux.HandleFunc("GET /admin/alerts", s.handleAlerts)
	mux.HandleFunc("GET /admin/queue", s.handleQueue)
	mux.HandleFunc("GET /admin/autoscale", s.handleAutoscale)
	mux.HandleFunc("GET /admin/cluster", s.handleCluster)
	mux.HandleFunc("GET /admin/rollouts", s.handleRollouts)
	mux.HandleFunc("POST /admin/rollouts/{model}/reset", s.handleRolloutReset)
	mux.HandleFunc("GET /admin/stats", s.handleStats)
	mux.HandleFunc("GET /admin/stats/history", s.handleStatsHistory)
	mux.HandleFunc("GET /admin/models", s.handleModels)
	// React 控制台（构建产物嵌入二进制）。/admin/ui 重定向到带斜杠形式，
	// 保证页面内相对资源路径正确解析。
	mux.Handle("GET /admin/ui/", webui.Handler("/admin/ui/"))
	mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
	})

	s.httpSrv = &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: requireAdminAuth(cfg.Server.AdminToken, mux),
		// 流式响应禁止全局写超时；读头超时防慢速攻击，读体超时防慢速上传。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
	}
	return s
}

// requireAdminAuth 保护全部 /admin/* 路由：
//  1. 强制 Bearer 令牌（恒定时间比较，防时序侧信道）；
//  2. 变更类方法强制 Content-Type: application/json（防跨站表单投递的 CSRF 前置）。
//
// requireAdminAuth 保护整个管理面。
// /admin/ui*（控制台静态壳）公开：SPA 在浏览器加载后才做登录、对 admin 数据
// 端点发 Bearer 请求——页面/JS 本身无敏感信息，数据仍全部受鉴权保护。
func requireAdminAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/admin/ui" || strings.HasPrefix(p, "/admin/ui/") {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(p, "/admin/") && p != "/admin" {
			next.ServeHTTP(w, r)
			return
		}
		if !validBearer(r.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			http.Error(w, "未授权：缺少或错误的 Authorization Bearer 令牌", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") && r.Body != nil && r.ContentLength != 0 {
				http.Error(w, "变更类请求的 Content-Type 必须为 application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// validBearer 恒定时间比较 Bearer 令牌。
func validBearer(header, want string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Handler 返回完整路由（测试与嵌入场景使用）。
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

// Run 启动监听并阻塞到 ctx 取消，随后优雅退出（等待在途请求完成，上限 30s）。
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("网关启动", "listen", s.cfg.Server.Listen)
		errCh <- s.httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		slog.Info("收到退出信号，开始优雅退出")
		return s.httpSrv.Shutdown(shutdownCtx)
	}
}

// backendView 后端状态视图。
type backendView struct {
	ID       string             `json:"id"`
	URL      string             `json:"url"`
	Engine   string             `json:"engine"`
	Weight   float64            `json:"weight"`
	Healthy  bool               `json:"healthy"`
	Cordoned bool               `json:"cordoned"`
	Ejected  bool               `json:"ejected"`
	Inflight int64              `json:"inflight"`
	Snapshot *backend.Snapshot  `json:"snapshot"`
	PromVars map[string]float64 `json:"prom_vars,omitempty"`
	Labels   map[string]string  `json:"labels,omitempty"`
}

func (s *Server) handleBackends(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	backends := s.reg.All()
	out := make([]backendView, 0, len(backends))
	for _, b := range backends {
		out = append(out, backendView{
			ID: b.ID, URL: b.URL.String(), Engine: string(b.Engine), Weight: b.Weight,
			Healthy: b.Healthy(), Cordoned: b.Cordoned(), Ejected: b.Ejected(now),
			Inflight: b.Inflight(), Snapshot: b.Snapshot(), PromVars: b.PromVars(), Labels: b.Labels,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// backendPayload POST /admin/backends 的请求体。
type backendPayload struct {
	ID             string            `json:"id"`
	URL            string            `json:"url"`
	Engine         string            `json:"engine"`
	Weight         float64           `json:"weight"`
	MaxConcurrency int               `json:"max_concurrency"`
	MetricsPath    string            `json:"metrics_path"`
	HealthPath     string            `json:"health_path"`
	BootstrapPort  int               `json:"bootstrap_port"`
	Labels         map[string]string `json:"labels"`
	// Models 该后端要加入的模型路由名（必填，须为非 PD 路由）。
	Models []string `json:"models"`
}

func (p *backendPayload) toConfig() config.BackendConfig {
	bc := config.BackendConfig{
		ID: p.ID, URL: p.URL, Engine: p.Engine, Weight: p.Weight,
		MaxConcurrency: p.MaxConcurrency, MetricsPath: p.MetricsPath,
		HealthPath: p.HealthPath, BootstrapPort: p.BootstrapPort, Labels: p.Labels,
	}
	bc.ApplyDefaults()
	return bc
}

// handleBackendPost 动态注册后端：注册表生效 -> 持久化 -> 集群广播。
// 持久化失败会回滚注册表（保证 DB 与运行态一致，重启不产生幽灵后端）。
func (s *Server) handleBackendPost(w http.ResponseWriter, r *http.Request) {
	var p backendPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	switch {
	case p.ID == "" || p.URL == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 与 url 必填"})
		return
	case !config.ValidEngine(p.Engine):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "engine 非法，可选 vllm/vllm_omni/sglang/sglang_omni"})
		return
	case len(p.Models) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "models 必填：后端要加入的模型路由名列表"})
		return
	}
	if s.reg.Get(p.ID) != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "后端已存在: " + p.ID})
		return
	}
	bc := p.toConfig()
	if _, err := s.reg.AddBackend(bc, p.Models); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.st != nil {
		if err := s.st.UpsertBackend(r.Context(), bc, p.Models); err != nil {
			_ = s.reg.RemoveBackend(bc.ID)
			slog.Error("后端注册持久化失败，已回滚", "backend", bc.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败，已回滚: " + err.Error()})
			return
		}
	}
	if s.clu != nil {
		if err := s.clu.PublishBackendChange(r.Context(), cluster.BackendUpsert, bc, p.Models); err != nil {
			slog.Error("后端注册广播失败，其余实例未同步", "backend", bc.ID, "err", err)
		}
	}
	slog.Info("动态注册后端（管理操作）", "backend", bc.ID, "url", bc.URL, "models", p.Models)
	writeJSON(w, http.StatusCreated, map[string]interface{}{"backend": bc.ID, "models": p.Models, "status": "registered"})
}

// handleBackendDelete 动态摘除后端。PD 组成员拓扑静态，拒绝摘除。
func (s *Server) handleBackendDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, g := range s.cfg.PDGroups {
		for _, member := range append(append([]string{}, g.Prefill...), g.Decode...) {
			if member == id {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "后端 " + id + " 属于 PD 组 " + g.Name + "，PD 拓扑不支持动态变更"})
				return
			}
		}
	}
	// 先删持久层再摘运行态：DB 删除失败时运行态保持不动（服务不中断），
	// 客户端可重试；避免旧顺序"运行态已摘除、DB 失败"造成重启后复活的不一致。
	if s.st != nil {
		if err := s.st.DeleteBackend(r.Context(), id); err != nil {
			slog.Error("后端持久层删除失败", "backend", id, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "持久层删除失败，运行态未摘除，请重试: " + err.Error()})
			return
		}
	}
	if err := s.reg.RemoveBackend(id); err != nil {
		if s.st != nil {
			// 注册表不存在该后端：DB 删除已成功，属幂等清除，不回滚。
			slog.Warn("注册表无此后端，DB 删除已生效（幂等）", "backend", id)
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	// M14：管理面摘除路径同样联动会话绑定清理（本地 + Redis 共享）。
	if s.sessStore != nil {
		s.sessStore.InvalidateBackend(id)
	}
	if s.clu != nil {
		if err := s.clu.PublishBackendChange(r.Context(), cluster.BackendDelete,
			config.BackendConfig{ID: id}, nil); err != nil {
			slog.Error("后端摘除广播失败，其余实例未同步", "backend", id, "err", err)
		}
	}
	slog.Info("动态摘除后端（管理操作）", "backend", id)
	writeJSON(w, http.StatusOK, map[string]string{"backend": id, "status": "removed"})
}

func (s *Server) handleCordon(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		b := s.reg.Get(id)
		if b == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "后端不存在: " + id})
			return
		}
		b.Cordon(on)
		slog.Info("后端隔离状态变更（管理操作）", "backend", id, "cordoned", on)
		writeJSON(w, http.StatusOK, map[string]interface{}{"backend": id, "cordoned": on})
	}
}

func (s *Server) handlePoliciesGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.pol.List())
}

// policyPayload PUT /admin/policies/{name} 的请求体。
type policyPayload struct {
	Filter string `json:"filter"`
	Score  string `json:"score"`
}

// handlePolicyValidate 只读校验策略表达式：编译但不注册、不持久化。
// 返回 200 + valid=false，而非 400，让前端可在两个编辑区分别展示编译错误；
// 400 仅保留给非法 JSON 等传输层错误。
func (s *Server) handlePolicyValidate(w http.ResponseWriter, r *http.Request) {
	var p policyPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	filter, errs := policy.Validate(p.Filter, p.Score)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"valid":   len(errs) == 0,
		"filter":  filter,
		"score":   p.Score,
		"errors":  errs,
		"warning": "校验仅保证语法与结果类型；vars/raw 动态键是否存在取决于运行时指标源",
	})
}

// handlePolicyPut 热更新策略：编译成功才替换，失败返回 400 且运行策略不变。
func (s *Server) handlePolicyPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var p policyPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if compileErr, persistErr := s.applyPolicySource(r.Context(), name, p.Filter, p.Score); compileErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": compileErr.Error()})
		return
	} else if persistErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "策略已生效，但持久化失败（重启后丢失），请重试: " + persistErr.Error()})
		return
	}
	slog.Info("策略热更新（管理操作）", "policy", name, "score", p.Score, "filter", p.Filter)
	writeJSON(w, http.StatusOK, map[string]string{"policy": name, "status": "updated"})
}

// applyPolicySource 策略表达式生效的统一通道（手工热更与预设切换共用）：
// 编译生效 → 持久化（失败告知调用方，内存态保留）→ 集群广播（失败仅记日志，
// 其余实例可重放或等 Redis 恢复后重新下发）。
func (s *Server) applyPolicySource(ctx context.Context, name, filter, score string) (compileErr, persistErr error) {
	if err := s.pol.Set(name, filter, score); err != nil {
		return err, nil
	}
	if s.st != nil {
		if err := s.st.UpsertPolicy(ctx, name, filter, score); err != nil {
			slog.Error("策略持久化失败", "policy", name, "err", err)
			return nil, err
		}
	}
	if s.clu != nil {
		if err := s.clu.PublishPolicy(ctx, name, filter, score); err != nil {
			slog.Error("策略广播失败，其余实例未同步", "policy", name, "err", err)
		}
	}
	return nil, nil
}

// handlePresetsGet 预设方案清单 + 各策略槽位当前生效方案（仅显式预设声明，
// 见 presetExplicit；手写表达式一律 custom —— L3：不反查、不误标）。
func (s *Server) handlePresetsGet(w http.ResponseWriter, _ *http.Request) {
	policies := map[string]map[string]string{}
	for name, src := range s.pol.List() {
		preset := "custom"
		if v, ok := s.presetExplicit.Load(name); ok {
			preset = v.(string)
		}
		policies[name] = map[string]string{
			"filter": src["filter"],
			"score":  src["score"],
			"preset": preset,
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"presets":  policy.Presets,
		"policies": policies,
	})
}

// handlePresetApply 一键切换调度方案：把预设表达式写入目标策略槽位
// （默认 "default"，可用 ?policy= 指定），复用策略热更通道
// （编译校验 / 持久化 / 集群广播全部生效）。
func (s *Server) handlePresetApply(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pr := policy.FindPreset(name)
	if pr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "预设方案不存在: " + name})
		return
	}
	target := r.URL.Query().Get("policy")
	if target == "" {
		target = config.DefaultPolicyName
	}
	if compileErr, persistErr := s.applyPolicySource(r.Context(), target, pr.Filter, pr.Score); compileErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": compileErr.Error()})
		return
	} else if persistErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "方案已生效，但持久化失败（重启后丢失），请重试: " + persistErr.Error()})
		return
	}
	slog.Info("调度方案切换（管理操作）", "preset", name, "policy", target)
	s.presetExplicit.Store(target, name)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"policy": target,
		"preset": name,
		"title":  pr.Title,
		"filter": pr.Filter,
		"score":  pr.Score,
		"status": "applied",
	})
}

// handleCluster 集群成员视图：实例列表、leader 标注与本实例身份。
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if s.clu == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	members, err := s.clu.Members(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "查询集群成员失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"self":    s.clu.ID(),
		"leader":  s.clu.IsLeader(),
		"members": members,
	})
}

// handleExplain 打分解释：GET /admin/explain?model=&prompt=&policy=&session=
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	model := q.Get("model")
	route, err := s.reg.Route(model)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	req := &scheduler.Request{
		Model:      model,
		PromptText: q.Get("prompt"),
		PromptLen:  len(q.Get("prompt")),
		SessionID:  q.Get("session"),
	}
	details := s.sched.Explain(route, q.Get("policy"), req)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":    model,
		"route":    route.Name,
		"strategy": route.Strategy,
		"policy":   route.PolicyName,
		"scores":   details,
	})
}

func (s *Server) handleKVCache(w http.ResponseWriter, _ *http.Request) {
	if s.tree == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	st := s.tree.Size()
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "stats": st})
}

// handleQueue 排队状态：全局深度、按模型（队列）深度与容量参数。
func (s *Server) handleQueue(w http.ResponseWriter, _ *http.Request) {
	enabled, total, byModel := s.px.QueueStats()
	if !enabled {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":   true,
		"max_depth": s.cfg.Queue.MaxDepth,
		"max_wait":  s.cfg.Queue.MaxWait.D().String(),
		"depth":     total,
		"by_model":  byModel,
	})
}

func (s *Server) handlePD(w http.ResponseWriter, _ *http.Request) {
	out := map[string]interface{}{}
	for name, g := range s.pdMgr.Groups() {
		out[name] = map[string]interface{}{
			"strategy": g.Strategy,
			"policy":   g.PolicyName,
			"prefill":  ids(g.Prefill),
			"decode":   ids(g.Decode),
			"links":    g.Links(),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAlerts 当前 firing/pending 的告警清单。
func (s *Server) handleAlerts(w http.ResponseWriter, _ *http.Request) {
	if s.alertEng == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"active":  s.alertEng.Active(),
	})
}

// handleAutoscale 各模型最近一次扩缩容建议。
func (s *Server) handleAutoscale(w http.ResponseWriter, _ *http.Request) {
	if s.scaler == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":         true,
		"recommendations": s.scaler.Recommendations(),
	})
}

// handleRollouts 金丝雀发布状态清单。
func (s *Server) handleRollouts(w http.ResponseWriter, _ *http.Request) {
	if s.ro == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "rollouts": s.ro.Status()})
}

// handleRolloutReset 重新从第一阶开始发布（回滚后人工恢复入口）。
func (s *Server) handleRolloutReset(w http.ResponseWriter, r *http.Request) {
	if s.ro == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "未配置金丝雀发布"})
		return
	}
	model := r.PathValue("model")
	if err := s.ro.Reset(model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	slog.Info("金丝雀发布重置（管理操作）", "model", model)
	writeJSON(w, http.StatusOK, map[string]string{"model": model, "status": "restarted"})
}

// backendSummary 后端池的即时汇总（控制台 KPI 用）。
type backendSummary struct {
	Total        int     `json:"total"`
	Healthy      int     `json:"healthy"`
	Available    int     `json:"available"`
	Cordoned     int     `json:"cordoned"`
	Ejected      int     `json:"ejected"`
	Inflight     int64   `json:"inflight"`
	Running      float64 `json:"running"`
	Waiting      float64 `json:"waiting"`
	KVUsage      float64 `json:"kv_usage"`
	HitRate      float64 `json:"hit_rate"`
	GenTokPerSec float64 `json:"gen_tok_per_sec"`
	// Samples 参与 KVUsage/HitRate 均值的后端数（直采失败的实例不计入）。
	Samples int `json:"samples"`
}

// summarizeBackends 汇总后端池：计数类求和，占用率与命中率取可用后端的算术平均。
func (s *Server) summarizeBackends() backendSummary {
	now := time.Now()
	var sum backendSummary
	for _, b := range s.reg.All() {
		sum.Total++
		healthy, cordoned, ejected := b.Healthy(), b.Cordoned(), b.Ejected(now)
		if healthy {
			sum.Healthy++
		}
		if cordoned {
			sum.Cordoned++
		}
		if ejected {
			sum.Ejected++
		}
		if healthy && !cordoned && !ejected {
			sum.Available++
		}
		sum.Inflight += b.Inflight()
		snap := b.Snapshot()
		if snap == nil || snap.Err != "" {
			continue
		}
		sum.Samples++
		sum.Running += snap.Running
		sum.Waiting += snap.Waiting
		sum.KVUsage += snap.KVUsage
		sum.HitRate += snap.HitRate
		sum.GenTokPerSec += snap.GenTokPerSec
	}
	if sum.Samples > 0 {
		sum.KVUsage /= float64(sum.Samples)
		sum.HitRate /= float64(sum.Samples)
	}
	return sum
}

// handleStats 网关级聚合统计：累计请求量/错误率/时延分布（与 /metrics 同源）
// + 后端池汇总 + 排队即时深度。控制台 KPI 的唯一数据来源。
func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	agg, err := s.gw.Aggregate()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "聚合自身指标失败: " + err.Error()})
		return
	}
	uptime := time.Since(s.started).Seconds()
	out := map[string]interface{}{
		"since":          s.started,
		"uptime_seconds": uptime,
		"aggregate":      agg,
		"backends":       s.summarizeBackends(),
	}
	// 平均 RPS 以进程存活时长为分母，明确区别于 history 中的瞬时速率。
	if uptime > 0 {
		out["avg_rps"] = agg.RequestsTotal / uptime
	}
	queueEnabled, depth, byModel := s.px.QueueStats()
	q := map[string]interface{}{"enabled": queueEnabled}
	if queueEnabled {
		q["depth"] = depth
		q["by_model"] = byModel
		q["max_depth"] = s.cfg.Queue.MaxDepth
	}
	out["queue"] = q
	if s.hist != nil {
		out["history_interval_seconds"] = s.hist.Interval().Seconds()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStatsHistory 时序采样：GET /admin/stats/history?minutes=60
// 数据来自进程内环形缓冲（重启清空），长周期时序请查 Prometheus。
func (s *Server) handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	if s.hist == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "minutes 需为正整数"})
			return
		}
		// 钳制上限（7 天）：杜绝 time.Duration(n)*time.Minute 的 int64 溢出
		// 回绕（超大 n 可能溢出为正或负，静默破坏时间窗口语义）。
		if n > 7*24*60 {
			n = 7 * 24 * 60
		}
		since = time.Now().Add(-time.Duration(n) * time.Minute)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":          true,
		"interval_seconds": s.hist.Interval().Seconds(),
		"capacity":         s.hist.Capacity(),
		"samples":          s.hist.Samples(since),
	})
}

// splitView 权重子池视图（金丝雀分流）。
type splitView struct {
	Name     string   `json:"name"`
	Strategy string   `json:"strategy,omitempty"`
	Policy   string   `json:"policy,omitempty"`
	Weight   float64  `json:"weight"`
	Hits     uint64   `json:"hits"`
	Backends []string `json:"backends"`
}

// modelView 模型路由视图：拓扑 + 池内实例健康计数。
type modelView struct {
	Name         string      `json:"name"`
	Strategy     string      `json:"strategy"`
	Policy       string      `json:"policy"`
	PDGroup      string      `json:"pd_group,omitempty"`
	RewriteModel string      `json:"rewrite_model,omitempty"`
	RateLimitQPS float64     `json:"rate_limit_qps,omitempty"`
	Backends     []string    `json:"backends"`
	Total        int         `json:"total"`
	Available    int         `json:"available"`
	Inflight     int64       `json:"inflight"`
	KVUsage      float64     `json:"kv_usage"`
	HitRate      float64     `json:"hit_rate"`
	Splits       []splitView `json:"splits,omitempty"`
}

// handleModels 模型路由清单：控制台据此得到「模型 → 实例」的权威映射
// （后端快照本身不含模型标签，一个实例可服务多个模型路由）。
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	out := make([]modelView, 0, len(s.reg.Routes()))
	for name, rt := range s.reg.Routes() {
		pool := rt.Pool()
		// 路由未指定策略槽位时调度器回退到 default，视图按运行时真实行为展示。
		policyName := rt.PolicyName
		if policyName == "" {
			policyName = config.DefaultPolicyName
		}
		v := modelView{
			Name: name, Strategy: rt.Strategy, Policy: policyName,
			PDGroup: rt.PDGroup, RewriteModel: rt.RewriteModel, RateLimitQPS: rt.QPS,
			Backends: ids(pool), Total: len(pool),
		}
		var samples int
		for _, b := range pool {
			if b.Healthy() && !b.Cordoned() && !b.Ejected(now) {
				v.Available++
			}
			v.Inflight += b.Inflight()
			if snap := b.Snapshot(); snap != nil && snap.Err == "" {
				samples++
				v.KVUsage += snap.KVUsage
				v.HitRate += snap.HitRate
			}
		}
		if samples > 0 {
			v.KVUsage /= float64(samples)
			v.HitRate /= float64(samples)
		}
		for _, sp := range rt.Splits {
			v.Splits = append(v.Splits, splitView{
				Name: sp.Name, Strategy: sp.Strategy, Policy: sp.PolicyName,
				Weight: sp.Weight(), Hits: sp.Hits.Load(), Backends: ids(sp.Pool()),
			})
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func ids(bs []*backend.Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
