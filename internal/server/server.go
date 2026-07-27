// Package server 组装 HTTP 接入层：数据面（/v1/*）、指标端点（/metrics）、
// 存活探针（/healthz）与管理面（/admin/*）。
// 使用 Go 1.22 标准库方法路由，不引入第三方 web 框架。
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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

	httpSrv *http.Server
}

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

// New 构造服务并注册全部路由。
func New(cfg *config.Config, reg *backend.Registry, pol *policy.Engine, sched *scheduler.Scheduler,
	tree *kvcache.Tree, pdMgr *pd.Manager, gw *metrics.Gateway, px *proxy.Handler) *Server {

	s := &Server{cfg: cfg, reg: reg, pol: pol, sched: sched, tree: tree, pdMgr: pdMgr, gw: gw, px: px}

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
	// React 控制台（构建产物嵌入二进制）。/admin/ui 重定向到带斜杠形式，
	// 保证页面内相对资源路径正确解析。
	mux.Handle("GET /admin/ui/", webui.Handler("/admin/ui/"))
	mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
	})

	s.httpSrv = &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: mux,
		// 流式响应禁止全局写超时；读头超时防慢速攻击。
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
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
	if err := s.reg.RemoveBackend(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if s.st != nil {
		if err := s.st.DeleteBackend(r.Context(), id); err != nil {
			// 运行态已摘除；持久层删除失败会导致重启后复活，明确告知调用方重试。
			slog.Error("后端持久层删除失败", "backend", id, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "已从运行时摘除，但持久层删除失败（重启后可能恢复），请重试: " + err.Error()})
			return
		}
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

// handlePresetsGet 预设方案清单 + 各策略槽位当前生效方案（表达式反查，
// 无需存状态，集群各实例判定天然一致）。前端据此渲染"一键切换"面板。
func (s *Server) handlePresetsGet(w http.ResponseWriter, _ *http.Request) {
	policies := map[string]map[string]string{}
	for name, src := range s.pol.List() {
		policies[name] = map[string]string{
			"filter": src["filter"],
			"score":  src["score"],
			"preset": policy.MatchPreset(src["filter"], src["score"]),
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
