// Package rollout 实现金丝雀自动升降级：对带 splits 分池的模型路由，
// 按 steps 阶梯自动爬升金丝雀子池权重，用表达式判据守门：
//
//	started --观察 interval 后 promote_expr 为真--> promoted(下一阶) ... --> completed(全量)
//	   任意求值周期 rollback_expr 为真 --> rolled_back(金丝雀权重清零，state=failed)
//
// 判据数据来自网关自身观测：proxy 每次转发结果与 TTFT 上报到本包的
// 子池滑动窗口（按发布阶段分窗，晋级/回滚后重置），金丝雀池与稳定池
// （其余子池聚合）各一份，表达式对比两者的错误率与时延分布。
//
// 权重变更仅作用于本实例内存态（Split.SetWeight 原子写）；多实例部署时
// 各实例独立执行同一份发布配置，以各自观测流量判据，步调最终一致；
// webhook 事件经 notifyGate（集群 leader 判定）去重，避免重复通知。
package rollout

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/expr-lang/expr/vm"

	"ai-gateway/internal/alerting"
	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
)

// 发布状态。
const (
	StateIdle      = "idle"      // 未启动（auto_start=false，等待 admin reset 触发）
	StateRunning   = "running"   // 放量中
	StateCompleted = "completed" // 已全量
	StateFailed    = "failed"    // 已回滚，等待人工 reset
)

// ttftSampleCap 每窗口 TTFT 样本上限（环形覆盖），p95 由样本排序估算。
const ttftSampleCap = 512

// window 单个子池在当前发布阶段内的观测窗口。
type window struct {
	mu       sync.Mutex
	requests int64
	errors   int64
	ttft     []float64
	ttftIdx  int
}

func (w *window) observeResult(code int) {
	w.mu.Lock()
	w.requests++
	if code >= 500 {
		w.errors++
	}
	w.mu.Unlock()
}

func (w *window) observeTTFT(sec float64) {
	w.mu.Lock()
	if len(w.ttft) < ttftSampleCap {
		w.ttft = append(w.ttft, sec)
	} else {
		w.ttft[w.ttftIdx%ttftSampleCap] = sec
		w.ttftIdx++
	}
	w.mu.Unlock()
}

func (w *window) reset() {
	w.mu.Lock()
	w.requests, w.errors = 0, 0
	w.ttft = w.ttft[:0]
	w.ttftIdx = 0
	w.mu.Unlock()
}

// snapshot 返回 (请求数, 错误率, ttft 均值, ttft p95)。无样本时时延项为 0。
func (w *window) snapshot() (float64, float64, float64, float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reqs := float64(w.requests)
	errRate := 0.0
	if w.requests > 0 {
		errRate = float64(w.errors) / float64(w.requests)
	}
	if len(w.ttft) == 0 {
		return reqs, errRate, 0, 0
	}
	sum := 0.0
	sorted := append([]float64(nil), w.ttft...)
	for _, v := range sorted {
		sum += v
	}
	sort.Float64s(sorted)
	p95 := sorted[int(math.Ceil(float64(len(sorted))*0.95))-1]
	return reqs, errRate, sum / float64(len(sorted)), p95
}

// Rollout 单个模型路由的发布实例。
type Rollout struct {
	cfg        config.RolloutConfig
	canary     *backend.Split
	others     []*backend.Split
	baseOthers []float64 // 其余子池的 YAML 基线权重（回滚/分摊按比例还原）
	promote    *vm.Program
	rollback   *vm.Program

	mu        sync.Mutex
	state     string
	stepIdx   int
	stepStart time.Time

	canaryWin *window
	stableWin *window
}

// Manager 发布管理器：持有全部发布实例与观测索引，实现 proxy 上报接口。
type Manager struct {
	rollouts []*Rollout
	byModel  map[string]*Rollout
	// byBackend 后端 ID -> 归属窗口（同一后端可参与多个发布的不同子池）。
	// 子池成员集合固定（registry 语义），启动期构建一次即可。
	byBackend map[string][]*window

	notifier   *alerting.Notifier // 可为 nil（未配置 webhook）
	notifyGate func() bool        // 可为 nil；集群模式传 leader 判定去重通知
}

// New 构造管理器：编译判据、解析子池、构建观测索引；auto_start 的发布立即
// 进入第一阶。任一发布配置解析失败即返回错误（启动期左移暴露）。
func New(cfgs []config.RolloutConfig, reg *backend.Registry, notifier *alerting.Notifier, notifyGate func() bool) (*Manager, error) {
	m := &Manager{
		byModel:    map[string]*Rollout{},
		byBackend:  map[string][]*window{},
		notifier:   notifier,
		notifyGate: notifyGate,
	}
	for _, rc := range cfgs {
		rt, err := reg.Route(rc.Model)
		if err != nil || rt.Name != rc.Model {
			return nil, fmt.Errorf("rollout 模型 %s 的路由不存在", rc.Model)
		}
		r := &Rollout{cfg: rc, state: StateIdle, canaryWin: &window{}, stableWin: &window{}}
		for _, sp := range rt.Splits {
			if sp.Name == rc.Canary {
				r.canary = sp
			} else {
				r.others = append(r.others, sp)
				r.baseOthers = append(r.baseOthers, sp.Weight())
			}
		}
		if r.canary == nil || len(r.others) == 0 {
			return nil, fmt.Errorf("rollout 模型 %s 的子池不满足要求（金丝雀 %s + 至少一个稳定池）", rc.Model, rc.Canary)
		}
		if r.promote, err = policy.CompileBool(rc.PromoteExpr); err != nil {
			return nil, fmt.Errorf("rollout 模型 %s 的 promote_expr 编译失败: %w", rc.Model, err)
		}
		if r.rollback, err = policy.CompileBool(rc.RollbackExpr); err != nil {
			return nil, fmt.Errorf("rollout 模型 %s 的 rollback_expr 编译失败: %w", rc.Model, err)
		}
		for _, b := range r.canary.Pool() {
			m.byBackend[b.ID] = append(m.byBackend[b.ID], r.canaryWin)
		}
		for _, sp := range r.others {
			for _, b := range sp.Pool() {
				m.byBackend[b.ID] = append(m.byBackend[b.ID], r.stableWin)
			}
		}
		m.rollouts = append(m.rollouts, r)
		m.byModel[rc.Model] = r
	}
	now := time.Now()
	for _, r := range m.rollouts {
		if *r.cfg.AutoStart {
			m.start(r, now)
		}
	}
	return m, nil
}

// ObserveResult 实现 proxy.ResultObserver：按后端归属记入子池窗口。
func (m *Manager) ObserveResult(backendID string, code int) {
	for _, w := range m.byBackend[backendID] {
		w.observeResult(code)
	}
}

// ObserveTTFT 实现 proxy.ResultObserver。
func (m *Manager) ObserveTTFT(backendID string, sec float64) {
	for _, w := range m.byBackend[backendID] {
		w.observeTTFT(sec)
	}
}

// Run 判据求值循环（每秒一轮），随 ctx 退出。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(time.Now())
		}
	}
}

// Tick 对全部放量中的发布做一轮判据求值。导出以便测试直接驱动。
func (m *Manager) Tick(now time.Time) {
	for _, r := range m.rollouts {
		m.tickOne(r, now)
	}
}

func (m *Manager) tickOne(r *Rollout, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateRunning {
		return
	}
	env := r.env(now)

	// 回滚判据：任意求值周期命中立即回滚（不等观察期满）。
	if evalBool(r.rollback, env, r.cfg.Model, "rollback_expr") {
		r.applyWeights(0)
		r.state = StateFailed
		slog.Warn("金丝雀回滚", "model", r.cfg.Model, "step", r.stepIdx, "weight", r.cfg.Steps[r.stepIdx])
		m.notify(r, "rolled_back",
			fmt.Sprintf("模型 %s 金丝雀在 %.0f%% 档命中回滚判据，权重已清零", r.cfg.Model, r.cfg.Steps[r.stepIdx]), env)
		return
	}
	// 晋级判据：观察期满后每轮尝试，直到命中或回滚。
	if now.Sub(r.stepStart) < r.cfg.Interval.D() {
		return
	}
	if !evalBool(r.promote, env, r.cfg.Model, "promote_expr") {
		return
	}
	r.stepIdx++
	if r.stepIdx >= len(r.cfg.Steps) {
		r.state = StateCompleted
		slog.Info("金丝雀发布完成", "model", r.cfg.Model)
		m.notify(r, "completed", fmt.Sprintf("模型 %s 金丝雀发布完成（%.0f%%）",
			r.cfg.Model, r.cfg.Steps[len(r.cfg.Steps)-1]), env)
		return
	}
	prev := r.cfg.Steps[r.stepIdx-1]
	next := r.cfg.Steps[r.stepIdx]
	r.applyWeights(next)
	r.stepStart = now
	r.canaryWin.reset()
	r.stableWin.reset()
	slog.Info("金丝雀晋级", "model", r.cfg.Model, "from", prev, "to", next)
	m.notify(r, "promoted", fmt.Sprintf("模型 %s 金丝雀权重 %.0f%% → %.0f%%", r.cfg.Model, prev, next), env)
}

// start 从第一阶开始放量（New 的 auto_start 与 admin Reset 共用）。
func (m *Manager) start(r *Rollout, now time.Time) {
	r.mu.Lock()
	r.stepIdx = 0
	r.state = StateRunning
	r.stepStart = now
	r.canaryWin.reset()
	r.stableWin.reset()
	r.applyWeights(r.cfg.Steps[0])
	env := r.env(now)
	r.mu.Unlock()
	slog.Info("金丝雀发布启动", "model", r.cfg.Model, "weight", r.cfg.Steps[0])
	m.notify(r, "started", fmt.Sprintf("模型 %s 金丝雀发布启动，首阶权重 %.0f%%", r.cfg.Model, r.cfg.Steps[0]), env)
}

// Reset 重新从第一阶开始（failed/completed/idle/running 任意状态均可触发）。
func (m *Manager) Reset(model string) error {
	r, ok := m.byModel[model]
	if !ok {
		return fmt.Errorf("模型 %s 未配置金丝雀发布", model)
	}
	m.start(r, time.Now())
	return nil
}

// applyWeights 按金丝雀百分比施加权重：金丝雀 = pct，其余子池按基线比例
// 分摊 100-pct（调用方持有 r.mu）。
func (r *Rollout) applyWeights(pct float64) {
	r.canary.SetWeight(pct)
	rest := 100 - pct
	baseSum := 0.0
	for _, w := range r.baseOthers {
		baseSum += w
	}
	for i, sp := range r.others {
		if baseSum > 0 {
			sp.SetWeight(rest * r.baseOthers[i] / baseSum)
		} else {
			sp.SetWeight(rest / float64(len(r.others)))
		}
	}
}

// env 构建判据求值环境（调用方持有 r.mu）。
func (r *Rollout) env(now time.Time) map[string]interface{} {
	cReq, cErr, cAvg, cP95 := r.canaryWin.snapshot()
	sReq, sErr, sAvg, sP95 := r.stableWin.snapshot()
	return map[string]interface{}{
		"canary_requests":   cReq,
		"canary_error_rate": cErr,
		"canary_ttft_avg":   cAvg,
		"canary_ttft_p95":   cP95,
		"stable_requests":   sReq,
		"stable_error_rate": sErr,
		"stable_ttft_avg":   sAvg,
		"stable_ttft_p95":   sP95,
		"canary_weight":     r.canary.Weight(),
		"step":              float64(r.stepIdx),
		"elapsed":           now.Sub(r.stepStart).Seconds(),
	}
}

// evalBool 求值判据；失败按 false 处理（不晋级、不回滚，维持现状最安全）。
func evalBool(prog *vm.Program, env map[string]interface{}, model, field string) bool {
	out, err := vm.Run(prog, env)
	if err != nil {
		slog.Warn("金丝雀判据求值失败", "model", model, "field", field, "err", err)
		return false
	}
	b, _ := out.(bool)
	return b
}

// notify 推送发布事件（notifyGate 为集群 leader 判定，非 leader 不重复通知）。
func (m *Manager) notify(r *Rollout, status, summary string, env map[string]interface{}) {
	if m.notifier == nil || (m.notifyGate != nil && !m.notifyGate()) {
		return
	}
	vars := map[string]float64{}
	for k, v := range env {
		if f, ok := v.(float64); ok {
			vars[k] = f
		}
	}
	m.notifier.Send(alerting.Event{
		Type:     alerting.TypeRollout,
		Status:   status,
		Instance: r.cfg.Model,
		Summary:  summary,
		Vars:     vars,
	}, r.cfg.Webhooks)
}

// StatusView 单个发布的 admin 视图。
type StatusView struct {
	Model        string    `json:"model"`
	Canary       string    `json:"canary"`
	State        string    `json:"state"`
	StepIndex    int       `json:"step_index"`
	Steps        []float64 `json:"steps"`
	CanaryWeight float64   `json:"canary_weight"`
	StepSince    time.Time `json:"step_since"`
	// 当前阶段窗口统计。
	CanaryRequests  float64 `json:"canary_requests"`
	CanaryErrorRate float64 `json:"canary_error_rate"`
	CanaryTTFTP95   float64 `json:"canary_ttft_p95"`
	StableRequests  float64 `json:"stable_requests"`
	StableErrorRate float64 `json:"stable_error_rate"`
	StableTTFTP95   float64 `json:"stable_ttft_p95"`
}

// Status 全部发布的当前状态，供 GET /admin/rollouts。
func (m *Manager) Status() []StatusView {
	out := make([]StatusView, 0, len(m.rollouts))
	for _, r := range m.rollouts {
		r.mu.Lock()
		cReq, cErr, _, cP95 := r.canaryWin.snapshot()
		sReq, sErr, _, sP95 := r.stableWin.snapshot()
		out = append(out, StatusView{
			Model: r.cfg.Model, Canary: r.cfg.Canary, State: r.state,
			StepIndex: r.stepIdx, Steps: r.cfg.Steps,
			CanaryWeight: r.canary.Weight(), StepSince: r.stepStart,
			CanaryRequests: cReq, CanaryErrorRate: cErr, CanaryTTFTP95: cP95,
			StableRequests: sReq, StableErrorRate: sErr, StableTTFTP95: sP95,
		})
		r.mu.Unlock()
	}
	return out
}
