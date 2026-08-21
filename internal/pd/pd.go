// Package pd 实现 Prefill/Decode 分离（PD disaggregation）调度。
//
// 背景：PD 分离部署下，prefill 实例算完提示词后需把 KV cache 经
// NCCL/NIXL 链路传给 decode 实例。链路是否存在、当前有多少在途传输、
// 带宽多大，直接决定了 (prefill, decode) 组合的优劣。
//
// 本包负责：
//   - 维护 PD 组（prefill 池 / decode 池 / 链路拓扑），未显式配置链路时按全互联处理；
//   - 选路：prefill 侧复用调度器策略（含表达式），decode 侧在"与所选 prefill
//     有链路"的约束下，按 decode 负载 + 链路拥塞度打分取最小；
//   - 链路在途传输计数（Acquire/Release），暴露给指标与 admin API。
package pd

import (
	"fmt"
	"sync/atomic"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/scheduler"
)

// Link 一条 prefill -> decode 的 KV 传输链路。
type Link struct {
	Prefill       string  `json:"prefill"`
	Decode        string  `json:"decode"`
	BandwidthGbps float64 `json:"bandwidth_gbps"`

	inflight atomic.Int64
}

// Acquire 记一笔在途 KV 传输。
func (l *Link) Acquire() { l.inflight.Add(1) }

// Release 结束一笔在途 KV 传输。
func (l *Link) Release() { l.inflight.Add(-1) }

// Inflight 当前在途传输数。
func (l *Link) Inflight() int64 { return l.inflight.Load() }

// Group 一个 PD 分离组。
type Group struct {
	Name       string
	Strategy   string
	PolicyName string
	Prefill    []*backend.Backend
	Decode     []*backend.Backend

	linksByPrefill map[string][]*Link
	decodeByID     map[string]*backend.Backend
	prefillByID    map[string]*backend.Backend
	rr             atomic.Uint64
}

// Manager 全部 PD 组。
type Manager struct {
	groups map[string]*Group
}

// NewManager 由配置构建 PD 组；链路缺省全互联。
func NewManager(cfg *config.Config, reg *backend.Registry) (*Manager, error) {
	m := &Manager{groups: map[string]*Group{}}
	for _, gc := range cfg.PDGroups {
		g := &Group{
			Name:           gc.Name,
			Strategy:       gc.Strategy,
			PolicyName:     gc.Policy,
			linksByPrefill: map[string][]*Link{},
			decodeByID:     map[string]*backend.Backend{},
			prefillByID:    map[string]*backend.Backend{},
		}
		for _, id := range gc.Prefill {
			g.Prefill = append(g.Prefill, reg.Get(id))
			g.prefillByID[id] = reg.Get(id)
		}
		for _, id := range gc.Decode {
			d := reg.Get(id)
			g.Decode = append(g.Decode, d)
			g.decodeByID[id] = d
		}
		if len(gc.Links) > 0 {
			for _, lc := range gc.Links {
				link := &Link{Prefill: lc.Prefill, Decode: lc.Decode, BandwidthGbps: lc.BandwidthGbps}
				g.linksByPrefill[lc.Prefill] = append(g.linksByPrefill[lc.Prefill], link)
			}
		} else {
			// 未声明链路：按全互联建链，带宽取默认值。
			for _, p := range gc.Prefill {
				for _, d := range gc.Decode {
					g.linksByPrefill[p] = append(g.linksByPrefill[p], &Link{Prefill: p, Decode: d, BandwidthGbps: 100})
				}
			}
		}
		m.groups[g.Name] = g
	}
	return m, nil
}

// Get 按名取 PD 组。
func (m *Manager) Get(name string) *Group { return m.groups[name] }

// Groups 返回全部组。
func (m *Manager) Groups() map[string]*Group { return m.groups }

// Pick 选出一对 (prefill, decode) 及其链路。
// prefill 侧走调度器策略（表达式/缓存感知等全部可用）；
// decode 侧只在与所选 prefill 连通的实例里选，打分 = decode 综合负载 + 链路拥塞度。
// excludeDecode 为本次请求已失败过的 decode 集合（servePD 按故障侧排除重试）。
func (g *Group) Pick(s *scheduler.Scheduler, req *scheduler.Request, excludePrefill, excludeDecode map[string]bool) (*backend.Backend, *backend.Backend, *Link, error) {
	return g.PickPreferred(s, req, "", excludePrefill, excludeDecode)
}

// PrefillAvailable 判断 prefill 实例是否在组内且当前可用（会话粘性候选判定，H9）。
func (g *Group) PrefillAvailable(id string) bool {
	p := g.prefillByID[id]
	return p != nil && p.Available(time.Now())
}

// PickPreferred 会话粘性版 Pick：preferPrefill 非空时优先选择该 prefill 实例
// （须组内成员、未被排除、且通过调度器单候选选路——策略/表达式同样生效），
// 失败自动回退普通选路。H9：PD 会话粘性绑定 prefill 侧（KV 亲和：同样的提示词
// 前缀应再次命中已持有其 KV cache 的实例）。
func (g *Group) PickPreferred(s *scheduler.Scheduler, req *scheduler.Request, preferPrefill string, excludePrefill, excludeDecode map[string]bool) (*backend.Backend, *backend.Backend, *Link, error) {
	now := time.Now()
	var prefill *backend.Backend
	if preferPrefill != "" && (excludePrefill == nil || !excludePrefill[preferPrefill]) {
		if p := g.prefillByID[preferPrefill]; p != nil && p.Available(now) {
			if b, err := s.PickAmong([]*backend.Backend{p}, g.Strategy, g.PolicyName, req, excludePrefill, &g.rr); err == nil && b != nil && b.ID == preferPrefill {
				prefill = b
			}
		}
	}
	if prefill == nil {
		p, err := s.PickAmong(g.Prefill, g.Strategy, g.PolicyName, req, excludePrefill, &g.rr)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("PD 组 %s prefill 选路失败: %w", g.Name, err)
		}
		prefill = p
	}

	decode, link, err := g.pickDecode(prefill, now, excludeDecode)
	if err != nil {
		return nil, nil, nil, err
	}
	return prefill, decode, link, nil
}

// pickDecode 在选定 prefill 的链路中选最优 decode（负载 + 链路拥塞度）。
func (g *Group) pickDecode(prefill *backend.Backend, now time.Time, excludeDecode map[string]bool) (*backend.Backend, *Link, error) {
	links := g.linksByPrefill[prefill.ID]
	var bestLink *Link
	var bestDecode *backend.Backend
	bestScore := 0.0
	for _, l := range links {
		d := g.decodeByID[l.Decode]
		// Available 涵盖健康、人工隔离与被动摘除冷却期，
		// 保证转发失败 MarkFailure 的熔断对 decode 选路同样生效。
		if d == nil || !d.Available(now) || (excludeDecode != nil && excludeDecode[d.ID]) {
			continue
		}
		if d.MaxConcurrency > 0 && d.Inflight() >= d.MaxConcurrency {
			continue
		}
		snap := d.Snapshot()
		// 链路拥塞度：在途传输数 / 带宽；乘以经验系数使其与请求数量级可比。
		congestion := float64(l.Inflight()) / l.BandwidthGbps * 10.0
		score := float64(d.Inflight()) + snap.Running + snap.Waiting + congestion
		if bestDecode == nil || score < bestScore {
			bestDecode, bestLink, bestScore = d, l, score
		}
	}
	if bestDecode == nil {
		return nil, nil, fmt.Errorf("PD 组 %s: prefill %s 无可用 decode 链路", g.Name, prefill.ID)
	}
	return bestDecode, bestLink, nil
}

// LinkState 链路状态视图（admin API / 指标用）。
type LinkState struct {
	Prefill       string  `json:"prefill"`
	Decode        string  `json:"decode"`
	BandwidthGbps float64 `json:"bandwidth_gbps"`
	Inflight      int64   `json:"inflight"`
}

// Links 返回组内全部链路状态。
func (g *Group) Links() []LinkState {
	var out []LinkState
	for _, links := range g.linksByPrefill {
		for _, l := range links {
			out = append(out, LinkState{
				Prefill: l.Prefill, Decode: l.Decode,
				BandwidthGbps: l.BandwidthGbps, Inflight: l.Inflight(),
			})
		}
	}
	return out
}
