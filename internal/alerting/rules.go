// 告警规则引擎：周期求值 + 状态机 + webhook 通知。
//
// 每条规则对其作用域内的每个实例（后端或模型路由）独立维护状态：
//
//	inactive --表达式为真--> pending --持续 For--> firing --表达式为假--> resolved(通知) --> inactive
//
// firing 期间按 RepeatInterval 重复通知（0 表示只通知一次）。
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
)

// Engine 告警规则引擎。
type Engine struct {
	interval time.Duration
	reg      *backend.Registry
	notifier *Notifier
	rules    []*compiledRule

	mu     sync.Mutex
	states map[string]*alertState // 键为 "规则名|实例"

	// onFiring 指标回调：每条规则当前 firing 的实例数，可为 nil。
	onFiring func(rule string, count float64)
	// gate 求值门控（集群模式的 leader 判定），nil 表示不受限。
	gate func() bool
}

type compiledRule struct {
	cfg  config.AlertRuleConfig
	prog *vm.Program
}

type alertState struct {
	activeSince  time.Time // 表达式首次为真的时刻
	firing       bool
	lastNotified time.Time
	lastVars     map[string]float64
}

// NewEngine 编译全部规则并构造引擎；任一规则编译失败即返回错误（启动期暴露）。
func NewEngine(cfg config.AlertingConfig, reg *backend.Registry, notifier *Notifier,
	onFiring func(rule string, count float64)) (*Engine, error) {
	e := &Engine{
		interval: cfg.Interval.D(),
		reg:      reg,
		notifier: notifier,
		states:   map[string]*alertState{},
		onFiring: onFiring,
	}
	for _, rc := range cfg.Rules {
		prog, err := compileBool(rc.Expr)
		if err != nil {
			return nil, fmt.Errorf("告警规则 %s 编译失败: %w", rc.Name, err)
		}
		e.rules = append(e.rules, &compiledRule{cfg: rc, prog: prog})
	}
	return e, nil
}

// SetGate 设置求值门控（装配期调用）：集群部署时传入 leader 判定，
// 非 leader 实例跳过求值，避免同一告警被多实例重复通知。
func (e *Engine) SetGate(gate func() bool) { e.gate = gate }

// Run 周期求值，直到 ctx 取消。
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e.gate != nil && !e.gate() {
				// 失去 leader：清空本地告警状态并归零 firing 指标。
				// resolved 通知由新 leader 的状态机负责——旧 leader 若保留
				// firing 状态将永久冻结（自己不再求值、新 leader 又无此状态），
				// /admin/alerts 与 gateway_alerts_firing 会一直误报。
				e.resetStates()
				continue
			}
			e.Evaluate(time.Now())
		}
	}
}

// resetStates 清空全部告警状态并把各规则 firing 计数归零（幂等）。
func (e *Engine) resetStates() {
	e.mu.Lock()
	hadStates := len(e.states) > 0
	if hadStates {
		e.states = map[string]*alertState{}
	}
	e.mu.Unlock()
	if hadStates && e.onFiring != nil {
		for _, r := range e.rules {
			e.onFiring(r.cfg.Name, 0)
		}
	}
}

// Evaluate 对全部规则做一轮求值。导出以便测试与 admin 手动触发。
// 同一作用域的求值环境在本轮内只构建一次，规则之间共享。
func (e *Engine) Evaluate(now time.Time) {
	envCache := map[string]map[string]map[string]interface{}{}
	for _, r := range e.rules {
		envs, ok := envCache[r.cfg.Scope]
		if !ok {
			envs = e.instances(r.cfg.Scope, now)
			envCache[r.cfg.Scope] = envs
		}
		firingCount := 0
		for instance, env := range envs {
			if e.evalInstance(r, instance, env, now) {
				firingCount++
			}
		}
		if e.onFiring != nil {
			e.onFiring(r.cfg.Name, float64(firingCount))
		}
	}
}

// instances 按作用域枚举求值实例及其环境。
func (e *Engine) instances(scope string, now time.Time) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	switch scope {
	case "backend":
		for _, b := range e.reg.All() {
			out[b.ID] = BackendEnv(b, now)
		}
	case "cluster":
		for name, rt := range e.reg.Routes() {
			out[name] = ClusterEnv(name, rt.Pool(), now)
		}
	}
	return out
}

// evalInstance 求值单个实例并驱动状态机，返回该实例当前是否 firing。
func (e *Engine) evalInstance(r *compiledRule, instance string, env map[string]interface{}, now time.Time) bool {
	active := false
	if out, err := vm.Run(r.prog, env); err != nil {
		// 求值失败按"不满足"处理，只记日志，不打断整轮求值。
		slog.Warn("告警规则求值失败", "rule", r.cfg.Name, "instance", instance, "err", err)
	} else if b, ok := out.(bool); ok {
		active = b
	}

	key := r.cfg.Name + "|" + instance
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.states[key]

	if !active {
		if st != nil {
			if st.firing {
				e.notify(r, instance, StatusResolved, st, now)
			}
			delete(e.states, key)
		}
		return false
	}

	if st == nil {
		st = &alertState{activeSince: now}
		e.states[key] = st
	}
	st.lastVars = numericVars(env)
	if !st.firing {
		if now.Sub(st.activeSince) >= r.cfg.For.D() {
			st.firing = true
			st.lastNotified = now
			e.notify(r, instance, StatusFiring, st, now)
		}
	} else if rep := r.cfg.RepeatInterval.D(); rep > 0 && now.Sub(st.lastNotified) >= rep {
		st.lastNotified = now
		e.notify(r, instance, StatusFiring, st, now)
	}
	return st.firing
}

// notify 构造事件并投递（调用方持有锁，Send 非阻塞所以安全）。
func (e *Engine) notify(r *compiledRule, instance, status string, st *alertState, now time.Time) {
	ev := Event{
		Type:     TypeAlert,
		Status:   status,
		Rule:     r.cfg.Name,
		Severity: r.cfg.Severity,
		Scope:    r.cfg.Scope,
		Instance: instance,
		Expr:     r.cfg.Expr,
		Summary:  r.cfg.Summary,
		StartsAt: st.activeSince,
		Labels:   r.cfg.Labels,
		Vars:     st.lastVars,
	}
	if status == StatusResolved {
		ev.EndsAt = now
	}
	e.notifier.Send(ev, r.cfg.Webhooks)
}

// ActiveAlert 为 admin 视图中的一条活动告警。
type ActiveAlert struct {
	Rule     string             `json:"rule"`
	Instance string             `json:"instance"`
	Scope    string             `json:"scope"`
	Severity string             `json:"severity"`
	State    string             `json:"state"` // pending | firing
	Since    time.Time          `json:"since"`
	Vars     map[string]float64 `json:"vars,omitempty"`
}

// Active 返回全部 pending/firing 状态的告警，供 GET /admin/alerts。
func (e *Engine) Active() []ActiveAlert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ActiveAlert, 0, len(e.states))
	for key, st := range e.states {
		var ruleName, instance string
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				ruleName, instance = key[:i], key[i+1:]
				break
			}
		}
		state := "pending"
		if st.firing {
			state = "firing"
		}
		var cfg config.AlertRuleConfig
		for _, r := range e.rules {
			if r.cfg.Name == ruleName {
				cfg = r.cfg
				break
			}
		}
		out = append(out, ActiveAlert{
			Rule: ruleName, Instance: instance, Scope: cfg.Scope, Severity: cfg.Severity,
			State: state, Since: st.activeSince, Vars: st.lastVars,
		})
	}
	return out
}
