// Package reconcile 实现动态配置的周期对账：以持久层（DB）为动态后端与
// 策略热更新的唯一事实来源，把本实例错过的集群广播收敛回本地状态。
//
// 为什么需要对账：集群广播走 Redis pub/sub（fire-and-forget），实例与 Redis
// 断连期间发布的消息永久丢失，重连后不补发——没有对账时状态漂移会持续到
// 实例重启。每个实例独立对账（不做 leader 门控：漂移是每实例各自的状态）。
//
// 收敛规则：
//   - DB 有、本地无（或配置/路由挂载不一致）的后端 → Upsert；
//   - 本地有、DB 无、且不属于 YAML 基线的后端（即动态注册的）→ 摘除；
//     YAML 基线后端不受"DB 缺失即摘除"影响；
//   - DB 中的策略与本地编译源码不一致 → 重新 Set（编译失败只告警）。
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/store"
)

// Reconciler 动态配置对账器。
type Reconciler struct {
	st       *store.Store
	reg      *backend.Registry
	pol      *policy.Engine
	yamlIDs  map[string]bool // YAML 基线后端 ID 集合
	interval time.Duration
}

// New 构造对账器。yamlBackends 传 YAML 配置中的后端清单（基线保护）。
func New(st *store.Store, reg *backend.Registry, pol *policy.Engine,
	yamlBackends []config.BackendConfig, interval time.Duration) *Reconciler {
	ids := make(map[string]bool, len(yamlBackends))
	for _, bc := range yamlBackends {
		ids[bc.ID] = true
	}
	return &Reconciler{st: st, reg: reg, pol: pol, yamlIDs: ids, interval: interval}
}

// Run 周期对账，直到 ctx 取消。
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Once(ctx)
		}
	}
}

// Once 执行一轮对账。导出以便测试直接驱动。
// DB 读取失败只告警跳过本轮：对账是收敛机制，短暂失败由下一轮补偿。
func (r *Reconciler) Once(ctx context.Context) {
	rows, err := r.st.ListBackends(ctx)
	if err != nil {
		slog.Warn("对账加载持久化后端失败，跳过本轮", "err", err)
	} else {
		r.reconcileBackends(rows)
	}

	pols, err := r.st.ListPolicies(ctx)
	if err != nil {
		slog.Warn("对账加载持久化策略失败，跳过本轮", "err", err)
		return
	}
	r.reconcilePolicies(pols)
}

// reconcileBackends 把 DB 后端集合收敛到本地注册表。
func (r *Reconciler) reconcileBackends(rows []store.BackendRow) {
	inDB := make(map[string]bool, len(rows))
	for _, row := range rows {
		inDB[row.Backend.ID] = true
		if r.backendSynced(row) {
			continue
		}
		if _, err := r.reg.UpsertBackend(row.Backend, row.Models); err != nil {
			slog.Warn("对账应用后端失败", "backend", row.Backend.ID, "err", err)
		} else {
			slog.Info("对账收敛后端（补齐错过的变更广播）", "backend", row.Backend.ID, "models", row.Models)
		}
	}
	// 动态注册的后端在 DB 中已不存在 → 错过了删除广播，摘除。
	for _, b := range r.reg.All() {
		if r.yamlIDs[b.ID] || inDB[b.ID] {
			continue
		}
		if err := r.reg.RemoveBackend(b.ID); err == nil {
			slog.Info("对账摘除后端（补齐错过的删除广播）", "backend", b.ID)
		}
	}
}

// backendSynced 判断本地实例与 DB 行是否一致：静态配置逐字段比较
// （DB 行与运行实例出自同一 ApplyDefaults 后的配置，同步时逐字段相等），
// 且行声明的每个模型路由的池都包含该后端。
func (r *Reconciler) backendSynced(row store.BackendRow) bool {
	b := r.reg.Get(row.Backend.ID)
	if b == nil {
		return false
	}
	bc := row.Backend
	if b.URL.String() != bc.URL || string(b.Engine) != bc.Engine ||
		b.Weight != bc.Weight || b.MaxConcurrency != int64(bc.MaxConcurrency) ||
		b.MetricsPath != bc.MetricsPath || b.HealthPath != bc.HealthPath ||
		b.BootstrapPort != bc.BootstrapPort || !labelsEqual(b.Labels, bc.Labels) {
		return false
	}
	for _, m := range row.Models {
		rt, ok := r.reg.Routes()[m]
		if !ok {
			// 路由在本实例不存在（YAML 版本不一致）：交给 Upsert 报错留痕。
			return false
		}
		if !poolHas(rt.Pool(), b) {
			return false
		}
	}
	return true
}

// reconcilePolicies 把 DB 策略收敛到本地策略引擎。
func (r *Reconciler) reconcilePolicies(pols map[string]config.PolicyConfig) {
	current := r.pol.List()
	for name, pc := range pols {
		// 引擎对空 filter 归一化为 "healthy"，比较前做同样归一化。
		filter := pc.Filter
		if filter == "" {
			filter = "healthy"
		}
		if cur, ok := current[name]; ok && cur["filter"] == filter && cur["score"] == pc.Score {
			continue
		}
		if err := r.pol.Set(name, pc.Filter, pc.Score); err != nil {
			slog.Warn("对账应用策略失败", "policy", name, "err", err)
		} else {
			slog.Info("对账收敛策略（补齐错过的热更新广播）", "policy", name)
		}
	}
}

// labelsEqual 比较标签表（nil 与空表视为相等）。
func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// poolHas 判断池中是否包含该实例（按指针：Upsert 换代后旧指针即视为未同步）。
func poolHas(pool []*backend.Backend, b *backend.Backend) bool {
	for _, x := range pool {
		if x == b {
			return true
		}
	}
	return false
}
