// 注册表：持有全部后端实例与模型路由。启动期由配置构建，运行期支持
// 动态增删后端（admin API / 集群广播 / 数据库持久层加载）。
//
// 并发模型：注册表自身（byID/all）由 RWMutex 保护；路由与子池的后端池
// 采用 copy-on-write（atomic.Pointer 切片整体替换），请求热路径经 Pool()
// 无锁读取，增删后端时整体换新切片，读写互不阻塞。
package backend

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"

	"ai-gateway/internal/config"
)

// Route 一条已解析的模型路由（配置中的引用已换成实例指针）。
type Route struct {
	Name       string
	Strategy   string
	PolicyName string
	PDGroup    string
	// Splits 权重子池（金丝雀分流），为空表示不分池。子池成员集合固定，
	// 仅当成员后端被移除时收缩。
	Splits []*Split
	// RewriteModel 转发前把请求体 model 字段改写为该值，空表示不改写。
	RewriteModel string
	QPS          float64
	Burst        int

	// RR round_robin 策略的游标。
	RR atomic.Uint64

	// pool 全池（copy-on-write）：无分池时即配置的 backends；
	// 有分池时为各子池的并集（去重）。
	pool atomic.Pointer[[]*Backend]
}

// Pool 返回当前后端池快照（只读切片，勿修改）。
func (r *Route) Pool() []*Backend {
	if p := r.pool.Load(); p != nil {
		return *p
	}
	return nil
}

// SetPool 整体替换后端池（注册表增删后端与测试装配使用）。
func (r *Route) SetPool(bs []*Backend) { r.pool.Store(&bs) }

// Split 一个权重子池及其运行态。
type Split struct {
	Name       string
	Strategy   string
	PolicyName string

	// weight 分流权重（float64 位模式存原子量）：调度热路径并发读，
	// 金丝雀自动升降级（internal/rollout）运行期改写。
	weight atomic.Uint64

	// RR 子池内 round_robin 游标；Hits 子池被选中次数（指标透出用）。
	RR   atomic.Uint64
	Hits atomic.Uint64

	pool atomic.Pointer[[]*Backend]
}

// Weight 当前分流权重。
func (s *Split) Weight() float64 { return math.Float64frombits(s.weight.Load()) }

// SetWeight 原子改写分流权重（金丝雀升降级用）。
func (s *Split) SetWeight(v float64) { s.weight.Store(math.Float64bits(v)) }

// Pool 返回子池后端快照。
func (s *Split) Pool() []*Backend {
	if p := s.pool.Load(); p != nil {
		return *p
	}
	return nil
}

// SetPool 整体替换子池。
func (s *Split) SetPool(bs []*Backend) { s.pool.Store(&bs) }

// PickSplit 按权重随机选一个子池；未配置分池时返回 nil。
func (r *Route) PickSplit() *Split {
	if len(r.Splits) == 0 {
		return nil
	}
	var total float64
	for _, sp := range r.Splits {
		total += sp.Weight()
	}
	if total <= 0 {
		return nil // 全部子池权重为零（如金丝雀回滚且稳定池未恢复）：回退全池选路
	}
	x := rand.Float64() * total
	for _, sp := range r.Splits {
		x -= sp.Weight()
		if x <= 0 && sp.Weight() > 0 {
			return sp
		}
	}
	return r.Splits[len(r.Splits)-1]
}

// Registry 后端与路由注册表。
type Registry struct {
	mu           sync.RWMutex
	byID         map[string]*Backend
	all          []*Backend
	routes       map[string]*Route
	defaultRoute *Route
}

// NewRegistry 由配置构建注册表。
func NewRegistry(cfg *config.Config) (*Registry, error) {
	r := &Registry{
		byID:   map[string]*Backend{},
		routes: map[string]*Route{},
	}
	for _, bc := range cfg.Backends {
		b, err := New(bc)
		if err != nil {
			return nil, err
		}
		r.byID[b.ID] = b
		r.all = append(r.all, b)
	}
	for _, mc := range cfg.Models {
		rt := &Route{
			Name:         mc.Name,
			Strategy:     mc.Strategy,
			PolicyName:   mc.Policy,
			PDGroup:      mc.PDGroup,
			RewriteModel: mc.RewriteModel,
			QPS:          mc.RateLimitQPS,
			Burst:        mc.RateLimitBurst,
		}
		var pool []*Backend
		for _, id := range mc.Backends {
			pool = append(pool, r.byID[id])
		}
		// 分池：构建子池并把成员并入全池（去重）。
		seen := map[string]bool{}
		for _, b := range pool {
			seen[b.ID] = true
		}
		for _, sc := range mc.Splits {
			sp := &Split{Name: sc.Name, Strategy: sc.Strategy, PolicyName: sc.Policy}
			sp.SetWeight(sc.Weight)
			var spool []*Backend
			for _, id := range sc.Backends {
				b := r.byID[id]
				spool = append(spool, b)
				if !seen[b.ID] {
					seen[b.ID] = true
					pool = append(pool, b)
				}
			}
			sp.SetPool(spool)
			rt.Splits = append(rt.Splits, sp)
		}
		rt.SetPool(pool)
		r.routes[mc.Name] = rt
		if mc.Name == "*" {
			r.defaultRoute = rt
		}
	}
	return r, nil
}

// Get 按 ID 查后端，不存在返回 nil。
func (r *Registry) Get(id string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

// All 返回全部后端快照。
func (r *Registry) All() []*Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Backend, len(r.all))
	copy(out, r.all)
	return out
}

// Route 按模型名查路由，未命中时回退 "*" 兜底路由；两者皆无返回错误。
func (r *Registry) Route(model string) (*Route, error) {
	if rt, ok := r.routes[model]; ok {
		return rt, nil
	}
	if r.defaultRoute != nil {
		return r.defaultRoute, nil
	}
	return nil, fmt.Errorf("模型 %q 无路由且未配置兜底路由 \"*\"", model)
}

// Routes 返回全部路由（路由集合本身固定，池内容动态）。
func (r *Registry) Routes() map[string]*Route { return r.routes }

// AddBackend 动态注册后端并加入指定模型路由的池。
// 约束：ID 不得重复；目标路由必须存在且非 PD 路由（PD 拓扑静态，需重启变更）。
func (r *Registry) AddBackend(bc config.BackendConfig, models []string) (*Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[bc.ID]; ok {
		return nil, fmt.Errorf("动态注册后端失败: id 已存在: %s", bc.ID)
	}
	return r.upsertLocked(bc, models)
}

// UpsertBackend 幂等注册（集群广播与持久层启动加载的应用入口）：
// 单次持锁内完成"校验 → 摘旧 → 加新"，任一步校验失败都不改变现有状态，
// 也不存在两段锁之间的空窗。
func (r *Registry) UpsertBackend(bc config.BackendConfig, models []string) (*Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upsertLocked(bc, models)
}

// upsertLocked 持锁执行注册/替换：先完成全部校验与新实例构造，
// 全部成功后才摘除同 ID 旧实例并加入新实例。
func (r *Registry) upsertLocked(bc config.BackendConfig, models []string) (*Backend, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("动态注册后端 %s 失败: 必须指定至少一个模型路由", bc.ID)
	}
	targets := make([]*Route, 0, len(models))
	for _, m := range models {
		rt, ok := r.routes[m]
		if !ok {
			return nil, fmt.Errorf("动态注册后端 %s 失败: 模型路由不存在: %s", bc.ID, m)
		}
		if rt.PDGroup != "" {
			return nil, fmt.Errorf("动态注册后端 %s 失败: 模型 %s 为 PD 路由，PD 拓扑不支持动态变更", bc.ID, m)
		}
		targets = append(targets, rt)
	}
	b, err := New(bc)
	if err != nil {
		return nil, err
	}
	if old, ok := r.byID[bc.ID]; ok {
		r.removeLocked(old)
	}
	r.byID[b.ID] = b
	r.all = append(r.all, b)
	for _, rt := range targets {
		pool := append(append([]*Backend{}, rt.Pool()...), b)
		rt.SetPool(pool)
	}
	return b, nil
}

// RemoveBackend 动态摘除后端：从注册表、全部路由池与子池中移除。
// 在途请求持有的实例指针不受影响，正常完成后该后端不再可被选中。
func (r *Registry) RemoveBackend(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("动态摘除后端失败: 不存在: %s", id)
	}
	r.removeLocked(b)
	return nil
}

// removeLocked 持锁摘除：从注册表、全部路由池与子池中移除实例。
func (r *Registry) removeLocked(b *Backend) {
	delete(r.byID, b.ID)
	all := make([]*Backend, 0, len(r.all)-1)
	for _, x := range r.all {
		if x != b {
			all = append(all, x)
		}
	}
	r.all = all
	for _, rt := range r.routes {
		rt.SetPool(without(rt.Pool(), b))
		for _, sp := range rt.Splits {
			sp.SetPool(without(sp.Pool(), b))
		}
	}
}

// without 返回剔除指定后端后的新切片（原切片不动，copy-on-write）。
func without(pool []*Backend, b *Backend) []*Backend {
	out := make([]*Backend, 0, len(pool))
	for _, x := range pool {
		if x != b {
			out = append(out, x)
		}
	}
	return out
}
