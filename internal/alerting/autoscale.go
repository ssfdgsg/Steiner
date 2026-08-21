// 自动扩缩容建议器：按模型路由周期求值扩/缩表达式，产出期望副本数建议。
//
// 网关只发信号、不执行：建议经 webhook（generic 模板）推送给外部控制器
// （K8s operator、运维机器人），同时暴露在 GET /admin/autoscale 与
// gateway_autoscale_desired_replicas 指标上（可作为 HPA/KEDA 的外部指标源）。
// 扩容后的新实例需加入网关配置并重载后才会接流。
package alerting

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/expr-lang/expr/vm"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
)

// Recommendation 一条扩缩容建议（admin 视图）。
type Recommendation struct {
	Model             string             `json:"model"`
	CurrentReplicas   int                `json:"current_replicas"`
	AvailableReplicas int                `json:"available_replicas"`
	DesiredReplicas   int                `json:"desired_replicas"`
	Direction         string             `json:"direction"` // up | down | hold
	Reason            string             `json:"reason"`
	Time              time.Time          `json:"time"`
	Vars              map[string]float64 `json:"vars,omitempty"`
}

// Autoscaler 扩缩容建议器。
type Autoscaler struct {
	cfg      config.AutoscaleConfig
	reg      *backend.Registry
	notifier *Notifier
	policies []*scalePolicy

	mu       sync.Mutex
	last     map[string]Recommendation // 最近一次建议，键为模型名
	lastUp   map[string]time.Time      // 各模型上次扩容建议时间（冷却用）
	lastDown map[string]time.Time
	// produced 最近产出过非 hold 建议的模型集合：失去 leader 时用于
	// 逐个回调 onDesired(model, 0) 归零指标，防止 HPA 按陈旧建议扩容。
	produced map[string]struct{}

	// onDesired 指标回调：模型 -> 期望副本数，可为 nil。
	onDesired func(model string, desired float64)

	// gate 求值门控（集群 leader 判定），可为 nil（单机始终求值）。
	gate func() bool
}

type scalePolicy struct {
	cfg      config.AutoscalePolicyConfig
	upProg   *vm.Program // 为 nil 表示该方向未启用
	downProg *vm.Program
}

// NewAutoscaler 编译全部策略表达式并构造建议器。
func NewAutoscaler(cfg config.AutoscaleConfig, reg *backend.Registry, notifier *Notifier,
	onDesired func(model string, desired float64)) (*Autoscaler, error) {
	a := &Autoscaler{
		cfg: cfg, reg: reg, notifier: notifier,
		last: map[string]Recommendation{}, lastUp: map[string]time.Time{},
		lastDown: map[string]time.Time{}, produced: map[string]struct{}{},
		onDesired: onDesired,
	}
	for _, pc := range cfg.Policies {
		sp := &scalePolicy{cfg: pc}
		var err error
		if pc.ScaleUpExpr != "" {
			if sp.upProg, err = compileBool(pc.ScaleUpExpr); err != nil {
				return nil, fmt.Errorf("autoscale 策略 %s 的 scale_up_expr 编译失败: %w", pc.Model, err)
			}
		}
		if pc.ScaleDownExpr != "" {
			if sp.downProg, err = compileBool(pc.ScaleDownExpr); err != nil {
				return nil, fmt.Errorf("autoscale 策略 %s 的 scale_down_expr 编译失败: %w", pc.Model, err)
			}
		}
		a.policies = append(a.policies, sp)
	}
	return a, nil
}

// compileBool 委托 policy.CompileBool：全项目布尔表达式方言只有一处定义。
func compileBool(src string) (*vm.Program, error) { return policy.CompileBool(src) }

// Run 周期求值，直到 ctx 取消。
func (a *Autoscaler) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Interval.D())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.gate != nil && !a.gate() {
				// 失去生产建议资格（非 leader）：把先前产出过建议的模型
				// 逐个回调 onDesired(model, 0) 归零，防止 HPA 按陈旧建议扩容。
				a.zeroStaleDesired()
				continue
			}
			a.Evaluate(time.Now())
		}
	}
}

// zeroStaleDesired 失去 leader 时归零各模型建议指标（幂等）：
// 只影响最近产出过建议的模型，且归零后清空该集合，避免重复回调。
func (a *Autoscaler) zeroStaleDesired() {
	a.mu.Lock()
	models := make([]string, 0, len(a.produced))
	for m := range a.produced {
		models = append(models, m)
	}
	clear(a.produced)
	a.mu.Unlock()
	if a.onDesired == nil {
		return
	}
	for _, m := range models {
		slog.Info("失去 leader，清空扩缩容建议指标", "model", m)
		a.onDesired(m, 0)
	}
}

// SetGate 设置求值门控（装配期调用）：集群部署时传入 leader 判定，
// 非 leader 实例跳过求值，避免重复产出扩缩容建议。
func (a *Autoscaler) SetGate(gate func() bool) { a.gate = gate }

// Evaluate 对全部策略做一轮求值。导出以便测试与 admin 手动触发。
func (a *Autoscaler) Evaluate(now time.Time) {
	for _, p := range a.policies {
		a.evalPolicy(p, now)
	}
}

// evalPolicy 求值单条策略：方向判定 -> 冷却检查 -> 钳位 -> 通知。
func (a *Autoscaler) evalPolicy(p *scalePolicy, now time.Time) {
	rt, err := a.reg.Route(p.cfg.Model)
	if err != nil {
		return // 配置校验已保证路由存在，兜底跳过
	}
	pool := rt.Pool()
	env := ClusterEnv(p.cfg.Model, pool, now)

	current := len(pool)
	available := 0
	for _, b := range pool {
		if b.Available(now) {
			available++
		}
	}

	up := a.evalBool(p.upProg, env, p.cfg.Model, "scale_up_expr")
	down := a.evalBool(p.downProg, env, p.cfg.Model, "scale_down_expr")

	rec := Recommendation{
		Model: p.cfg.Model, CurrentReplicas: current, AvailableReplicas: available,
		DesiredReplicas: current, Direction: "hold", Time: now, Vars: numericVars(env),
	}
	switch {
	case up: // 扩缩同时命中时扩容优先，保守偏向可用性
		rec.Direction = "up"
		rec.DesiredReplicas = current + p.cfg.ScaleUpStep
		if p.cfg.MaxReplicas > 0 && rec.DesiredReplicas > p.cfg.MaxReplicas {
			rec.DesiredReplicas = p.cfg.MaxReplicas
		}
		rec.Reason = "scale_up_expr 命中: " + p.cfg.ScaleUpExpr
	case down:
		rec.Direction = "down"
		rec.DesiredReplicas = current - p.cfg.ScaleDownStep
		if rec.DesiredReplicas < p.cfg.MinReplicas {
			rec.DesiredReplicas = p.cfg.MinReplicas
		}
		rec.Reason = "scale_down_expr 命中: " + p.cfg.ScaleDownExpr
	}
	if rec.DesiredReplicas == current {
		rec.Direction = "hold"
	}

	a.mu.Lock()
	a.last[p.cfg.Model] = rec
	if rec.Direction != "hold" {
		a.produced[p.cfg.Model] = struct{}{}
	}
	shouldNotify := false
	if rec.Direction == "up" && now.Sub(a.lastUp[p.cfg.Model]) >= p.cfg.ScaleUpCooldown.D() {
		a.lastUp[p.cfg.Model] = now
		shouldNotify = true
	}
	if rec.Direction == "down" && now.Sub(a.lastDown[p.cfg.Model]) >= p.cfg.ScaleDownCooldown.D() {
		a.lastDown[p.cfg.Model] = now
		shouldNotify = true
	}
	a.mu.Unlock()

	// M3: onDesired 指标回调与通知同一门控——冷却未通过就不产出建议、
	// 不更新 gateway_autoscale_desired_replicas 指标，保证指标与 webhook 一致
	// （冷却期只更新 admin 视图 a.last，见上方锁定区间）。
	if !shouldNotify {
		return
	}
	if a.onDesired != nil {
		a.onDesired(p.cfg.Model, float64(rec.DesiredReplicas))
	}
	slog.Info("产出扩缩容建议", "model", rec.Model, "direction", rec.Direction,
		"current", rec.CurrentReplicas, "desired", rec.DesiredReplicas)
	a.notifier.Send(Event{
		Type:    TypeAutoscale,
		Status:  StatusRecommendation,
		Summary: rec.Reason,
		Vars:    rec.Vars,
		Scale: &ScaleAdvice{
			Model: rec.Model, CurrentReplicas: rec.CurrentReplicas,
			AvailableReplicas: rec.AvailableReplicas, DesiredReplicas: rec.DesiredReplicas,
			Direction: rec.Direction,
		},
	}, a.cfg.Webhooks)
}

// evalBool 求值布尔程序；prog 为 nil 或求值失败均按 false 处理。
func (a *Autoscaler) evalBool(prog *vm.Program, env map[string]interface{}, model, field string) bool {
	if prog == nil {
		return false
	}
	out, err := vm.Run(prog, env)
	if err != nil {
		slog.Warn("autoscale 表达式求值失败", "model", model, "field", field, "err", err)
		return false
	}
	b, _ := out.(bool)
	return b
}

// Recommendations 返回各模型最近一次建议，供 GET /admin/autoscale。
func (a *Autoscaler) Recommendations() []Recommendation {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Recommendation, 0, len(a.last))
	for _, r := range a.last {
		out = append(out, r)
	}
	return out
}

// 首次冷却语义说明：lastUp/lastDown 零值时间意味着启动后的第一条建议
// 一定可以发出（now - 零值 >> cooldown），符合"尽快暴露容量问题"的预期。
