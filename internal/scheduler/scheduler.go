// Package scheduler 实现后端选路。支持 8 种策略：
//
//	round_robin      轮询
//	random           随机
//	weighted_random  按静态权重随机
//	least_request    最小负载（在途 + 运行 + 排队）
//	p2c              两次随机取负载较小者（Power of Two Choices）
//	consistent_hash  一致性哈希（会话/前缀亲和）
//	cache_aware      KV 前缀感知：命中率高走亲和，负载失衡回退最小负载
//	expression       策略表达式打分（动态编译，支持热更新）
//
// 所有策略共享同一候选过滤：健康、未隔离、不在熔断冷却期、未被本次重试排除、
// 并发额度未占满。
package scheduler

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/policy"
)

// Request 一次调度所需的请求侧信息，由代理层从请求体中提取。
type Request struct {
	Model      string
	PromptText string
	PromptLen  int
	Stream     bool
	SessionID  string
	Priority   float64

	// 多模态特征与成本估算（代理层按经验权重折算，见 proxy/parse.go）。
	IsMultimodal    bool
	ImageCount      int
	AudioCount      int
	VideoCount      int
	PromptTokensEst int
}

// Scheduler 调度器。
type Scheduler struct {
	pol   *policy.Engine
	tree  *kvcache.Tree // 未启用 KV 前缀感知时为 nil
	kvCfg config.KVCacheConfig

	// rings 一致性哈希环缓存，键为候选集合签名。
	ringMu sync.Mutex
	rings  map[string]*ring

	// ringsStable 修复后的哈希环缓存：键为稳定内容签名（候选 ID 排序哈希），
	// 值携带构建时的池快照用于"同 ID 换实例"检测。取代 ring.go 中基于
	// &pool[0] 地址的旧 pickHash/ringSignature（地址可被 GC 复用 → 陈旧环）。
	ringsStable map[string]*ringCacheEntry
}

// New 构造调度器。tree 可为 nil。
func New(pol *policy.Engine, tree *kvcache.Tree, kvCfg config.KVCacheConfig) *Scheduler {
	return &Scheduler{
		pol:         pol,
		tree:        tree,
		kvCfg:       kvCfg,
		rings:       map[string]*ring{},
		ringsStable: map[string]*ringCacheEntry{},
	}
}

// PromptRequirements 告诉代理层当前路由是否需要解析 prompt 特征，以及需要保留
// 多少字节的 PromptText。0 表示不保留，负数表示完整保留。
//
// 大多数负载/轮询策略只需要 model/stream 等小字段，可以走轻量信封解析；
// KV 前缀树最多使用 MaxPrefixBytes；无 session 的 consistent_hash 必须保留完整
// prompt 以维持既有哈希语义。
func (s *Scheduler) PromptRequirements(route *backend.Route, hasSession bool) (needFeatures bool, textLimit int) {
	if s.tree != nil {
		needFeatures = true
		textLimit = s.kvCfg.MaxPrefixBytes
	}
	check := func(strategy, policyName string) {
		switch strategy {
		case "consistent_hash":
			if !hasSession {
				needFeatures = true
				textLimit = -1
			}
		case "expression":
			if policyName == "" {
				policyName = config.DefaultPolicyName
			}
			if p := s.pol.Get(policyName); p != nil && policyUsesPromptFeatures(p) {
				needFeatures = true
			}
		}
	}
	check(route.Strategy, route.PolicyName)
	for _, sp := range route.Splits {
		check(sp.Strategy, sp.PolicyName)
	}
	return needFeatures, textLimit
}

var promptFeatureVariables = []string{
	"prompt_len", "prompt_tokens", "is_multimodal",
	"image_count", "audio_count", "video_count",
}

func policyUsesPromptFeatures(p *policy.Policy) bool {
	src := p.FilterSrc + "\n" + p.ScoreSrc
	for _, name := range promptFeatureVariables {
		if strings.Contains(src, name) {
			return true
		}
	}
	return false
}

// Pick 按路由配置选一个后端。exclude 为本次请求已失败过的后端集合。
// 配置了权重分池（金丝雀）时先按权重选子池、在子池内选路；
// 子池无可用后端时回退全池，可用性优先于分流比例。
func (s *Scheduler) Pick(route *backend.Route, req *Request, exclude map[string]bool) (*backend.Backend, error) {
	if sp := route.PickSplit(); sp != nil {
		if b, err := s.PickAmong(sp.Pool(), sp.Strategy, sp.PolicyName, req, exclude, &sp.RR); err == nil {
			sp.Hits.Add(1)
			return b, nil
		}
		slog.Debug("分池无可用后端，回退全池", "model", route.Name, "split", sp.Name)
	}
	return s.PickAmong(route.Pool(), route.Strategy, route.PolicyName, req, exclude, &route.RR)
}

// PickAmong 在给定候选池上执行策略选路；rr 为 round_robin 游标，可为 nil。
// PD 分离组的 prefill/decode 侧复用本方法。
func (s *Scheduler) PickAmong(pool []*backend.Backend, strategy, policyName string, req *Request, exclude map[string]bool, rr interface{ Add(uint64) uint64 }) (*backend.Backend, error) {
	now := time.Now()
	candidates := make([]*backend.Backend, 0, len(pool))
	for _, b := range pool {
		if exclude != nil && exclude[b.ID] {
			continue
		}
		if !b.Available(now) {
			continue
		}
		if b.MaxConcurrency > 0 && b.Inflight() >= b.MaxConcurrency {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("无可用后端（池大小 %d，全部不可用或已被排除）", len(pool))
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	switch strategy {
	case "round_robin":
		if rr == nil {
			return candidates[0], nil
		}
		return candidates[rr.Add(1)%uint64(len(candidates))], nil
	case "random":
		return candidates[rand.Intn(len(candidates))], nil
	case "weighted_random":
		return pickWeighted(candidates), nil
	case "least_request":
		return pickLeast(candidates), nil
	case "p2c":
		a := candidates[rand.Intn(len(candidates))]
		b := candidates[rand.Intn(len(candidates))]
		if load(b) < load(a) {
			return b, nil
		}
		return a, nil
	case "consistent_hash":
		return s.pickHashStable(pool, candidates, req)
	case "cache_aware":
		return s.pickCacheAware(candidates, req), nil
	case "expression":
		return s.pickExpression(candidates, policyName, req)
	default:
		return nil, fmt.Errorf("未知调度策略 %q", strategy)
	}
}

// Observe 请求成功转发后回写前缀归属，供后续 cache_aware / prefix_match 使用。
func (s *Scheduler) Observe(req *Request, backendID string) {
	if s.tree == nil || req.PromptText == "" {
		return
	}
	s.tree.Insert(req.PromptText, backendID)
}

// load 综合负载：网关侧在途 + 引擎运行中 + 引擎排队。
func load(b *backend.Backend) float64 {
	snap := b.Snapshot()
	return float64(b.Inflight()) + snap.Running + snap.Waiting
}

func pickLeast(candidates []*backend.Backend) *backend.Backend {
	best := candidates[0]
	bestLoad := load(best)
	for _, b := range candidates[1:] {
		if l := load(b); l < bestLoad {
			best, bestLoad = b, l
		}
	}
	return best
}

func pickWeighted(candidates []*backend.Backend) *backend.Backend {
	var total float64
	for _, b := range candidates {
		total += b.Weight
	}
	if total <= 0 {
		// M15：权重和≤0（全零或负权重，normalizeWeight 之外的旁路构造路径）
		// 时 rand.Float64()*total 恒为 0，旧代码要么落在首候选要么绕完整圈
		// 返回末位，权重语义完全失效。显式回退到首候选，行为确定。
		return candidates[0]
	}
	r := rand.Float64() * total
	for _, b := range candidates {
		r -= b.Weight
		if r <= 0 {
			return b
		}
	}
	return candidates[len(candidates)-1]
}

// ringCacheEntry 一致性哈希环缓存项：环本身 + 构建时的池快照。
// 快照用于"同 ID 换实例"检测：内容签名对相同 ID 集合稳定（这是签名设计的
// 目标——路由池增删顺序变化不重建环），但注册表 UpsertBackend 以同 ID 替换
// 实例后池内元素指针变化，若直接复用旧环会返回已摘除的旧实例指针。
type ringCacheEntry struct {
	rg   *ring
	pool []*backend.Backend
}

// ringSignatureContent 稳定内容签名：候选 ID 排序后拼接做 fnv64a 哈希。
// 取代 ring.go 旧 ringSignature 的 &pool[0] 地址签名——Go 不保证新池分配
// 不复用已回收数组的地址与长度，地址复用会命中陈旧环（一致性哈希对所有键
// 坍缩为固定首候选）。内容签名与底层数组地址、切片顺序无关；空池返回明确
// 错误而非 panic（正常调用链 PickAmong 有前置空检查，此处防御）。
func ringSignatureContent(pool []*backend.Backend) (string, error) {
	if len(pool) == 0 {
		return "", fmt.Errorf("一致性哈希签名失败:候选池为空")
	}
	ids := make([]string, len(pool))
	for i, b := range pool {
		ids[i] = b.ID
	}
	sort.Strings(ids)
	h := fnv.New64a()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0}) // 分隔符，避免 "ab"+"c" 与 "a"+"bc" 撞签名
	}
	return fmt.Sprintf("%d", h.Sum64()), nil
}

// sameInstances 逐元素指针身份比较：两个池快照是否指向完全相同的后端实例
// 集合（与顺序无关——环的落点由 ID 哈希决定，与池顺序无关）。
func sameInstances(a, b []*backend.Backend) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pickHashStable 一致性哈希选路（修复版）。环按"完整池"构建（保证键落点
// 稳定），可用性过滤在环上顺时针查找时进行；环缓存键为稳定内容签名。
// 与 ring.go 旧 pickHash 的区别：
//  1. 签名基于候选 ID 排序哈希而非 &pool[0] 地址——地址复用不再命中陈旧环；
//  2. 缓存项携带池快照，同 ID 换实例（upsert）时逐元素指针校验失败 → 重建，
//     杜绝返回已摘除的旧实例指针；
//  3. 空池返回显式错误而非对 &pool[0] 取地址 panic。
func (s *Scheduler) pickHashStable(pool, candidates []*backend.Backend, req *Request) (*backend.Backend, error) {
	key := req.SessionID
	if key == "" {
		key = req.PromptText
	}
	if key == "" {
		key = req.Model
	}

	sig, err := ringSignatureContent(pool)
	if err != nil {
		return nil, fmt.Errorf("一致性哈希选路: %w", err)
	}
	s.ringMu.Lock()
	entry, okCache := s.ringsStable[sig]
	if !okCache || !sameInstances(entry.pool, pool) {
		// 上限兜底：整体清空避免动态增删后端时缓存无界增长（环构建为微秒级）。
		if len(s.ringsStable) >= maxRingCache {
			s.ringsStable = map[string]*ringCacheEntry{}
		}
		entry = &ringCacheEntry{rg: buildRing(pool), pool: pool}
		s.ringsStable[sig] = entry
	}
	rg := entry.rg
	s.ringMu.Unlock()

	allowed := make(map[string]bool, len(candidates))
	for _, b := range candidates {
		allowed[b.ID] = true
	}
	if b := rg.get(key, func(b *backend.Backend) bool { return allowed[b.ID] }); b != nil {
		return b, nil
	}
	return candidates[0], nil
}

// prefixMatches 计算每个后端的前缀命中率（0~1）。
func (s *Scheduler) prefixMatches(req *Request) map[string]float64 {
	out := map[string]float64{}
	if s.tree == nil || req.PromptText == "" {
		return out
	}
	matches, keyLen := s.tree.Match(req.PromptText)
	if keyLen == 0 {
		return out
	}
	for id, n := range matches {
		out[id] = float64(n) / float64(keyLen)
	}
	return out
}

// pickCacheAware 复刻 sglang-router 的 cache-aware 决策：
// 最高前缀命中率达到阈值且目标后端未严重失衡时走亲和路由，否则回退最小负载。
func (s *Scheduler) pickCacheAware(candidates []*backend.Backend, req *Request) *backend.Backend {
	ratios := s.prefixMatches(req)
	var best *backend.Backend
	var bestRatio float64
	for _, b := range candidates {
		if r := ratios[b.ID]; r > bestRatio {
			best, bestRatio = b, r
		}
	}
	if best != nil && bestRatio >= s.kvCfg.MatchThreshold {
		minLoad := math.Inf(1)
		for _, b := range candidates {
			if l := load(b); l < minLoad {
				minLoad = l
			}
		}
		l := load(best)
		// 双阈值失衡判定：绝对负载超限且相对最小负载超比例才放弃亲和。
		if !(l > s.kvCfg.BalanceAbsThreshold && l > s.kvCfg.BalanceRelThreshold*minLoad) {
			return best
		}
	}
	return pickLeast(candidates)
}

// pickExpression 逐后端求值策略表达式，取分数（代价）最小者。
// 降级语义区分两类"无候选"：
//   - 求值错误数 >0 且无候选（H7）：filter 引用未知变量/除零/超时等运行期
//     错误导致候选被跳过，此时静默 pickLeast 会绕过 filter 约束，把请求发往
//     本应被过滤的后端 —— fail-closed，返回显式错误（请求方按 503 处理）；
//   - 全部候选被合法 filter 拒绝（pass=false 且零错误）：设计中的降级路径，
//     维持 pickLeast 兜底，保证请求不因策略过严而整体失败。
func (s *Scheduler) pickExpression(candidates []*backend.Backend, policyName string, req *Request) (*backend.Backend, error) {
	p := s.pol.Get(policyName)
	if p == nil {
		return nil, fmt.Errorf("策略 %q 不存在", policyName)
	}
	ratios := s.prefixMatches(req)
	var best *backend.Backend
	bestScore := math.Inf(1)
	var evalErrCount int
	var firstEvalErr error
	for _, b := range candidates {
		env := s.BuildEnv(req, b, ratios[b.ID])
		pass, score, err := p.Eval(env)
		if err != nil {
			evalErrCount++
			if firstEvalErr == nil {
				firstEvalErr = err
			}
			slog.Warn("策略求值失败，跳过该后端", "policy", policyName, "backend", b.ID, "err", err)
			continue
		}
		if pass && score < bestScore {
			best, bestScore = b, score
		}
	}
	if best == nil {
		if evalErrCount > 0 {
			slog.Error("策略表达式求值失败，拒绝降级绕过约束",
				"policy", policyName, "model", req.Model,
				"eval_err_count", evalErrCount, "first_err", firstEvalErr)
			return nil, fmt.Errorf("策略 %s 表达式求值失败，拒绝降级绕过约束", policyName)
		}
		slog.Warn("策略过滤后无候选，降级为最小负载", "policy", policyName, "model", req.Model)
		return pickLeast(candidates), nil
	}
	return best, nil
}

// BuildEnv 构建表达式求值环境。变量表见 README「策略表达式变量」一节。
func (s *Scheduler) BuildEnv(req *Request, b *backend.Backend, prefixMatch float64) map[string]interface{} {
	snap := b.Snapshot()
	return map[string]interface{}{
		// 请求侧
		"model":         req.Model,
		"stream":        req.Stream,
		"prompt_len":    float64(req.PromptLen),
		"prompt_tokens": float64(req.PromptTokensEst),
		"priority":      req.Priority,
		"session":       req.SessionID,
		"is_multimodal": req.IsMultimodal,
		"image_count":   float64(req.ImageCount),
		"audio_count":   float64(req.AudioCount),
		"video_count":   float64(req.VideoCount),
		// 后端侧
		"backend":       b.ID,
		"engine":        string(b.Engine),
		"engine_family": b.Engine.Family(),
		"weight":        b.Weight,
		"healthy":       b.Healthy(),
		"inflight":      float64(b.Inflight()),
		"running":       snap.Running,
		"waiting":       snap.Waiting,
		"kv_usage":      snap.KVUsage,
		"hit_rate":      snap.HitRate,
		"gen_tps":       snap.GenTokPerSec,
		"prefix_match":  prefixMatch,
		"labels":        b.Labels,
		// 时延与显存压力反馈（预设方案依赖，Go 侧预计算保证任何引擎下都存在）
		"ttft_ewma":    b.TTFTEWMA(),      // 网关实测首字节时延滑动均值（秒）
		"preempt_rate": preemptRate(snap), // 引擎抢占速率（次/秒），无抢占指标的引擎为 0
		// 扩展指标
		"raw":  snap.Raw,     // 后端 /metrics 原始指标（含 rate: 派生值）
		"vars": b.PromVars(), // 外部 Prometheus 注入变量
	}
}

// preemptRateCandidates 各引擎抢占计数 counter 的速率派生键（按序取首个命中）。
// PagedAttention 语义下抢占意味着整条请求 KV 被换出/重算，是最强的过载负反馈。
var preemptRateCandidates = []string{
	"rate:vllm:num_preemptions_total",
	"rate:sglang:num_preemptions_total",
}

// preemptRate 从原始指标中取抢占速率，缺失返回 0。
func preemptRate(snap *backend.Snapshot) float64 {
	for _, k := range preemptRateCandidates {
		if v, ok := snap.Raw[k]; ok {
			return v
		}
	}
	return 0
}

// ScoreDetail 单个后端的打分明细（admin 解释接口用）。
type ScoreDetail struct {
	Backend     string  `json:"backend"`
	Available   bool    `json:"available"`
	Pass        bool    `json:"pass"`
	Score       float64 `json:"score"`
	PrefixMatch float64 `json:"prefix_match"`
	Inflight    int64   `json:"inflight"`
	Running     float64 `json:"running"`
	Waiting     float64 `json:"waiting"`
	KVUsage     float64 `json:"kv_usage"`
	Err         string  `json:"err,omitempty"`
}

// Explain 对路由内全部后端做一次带明细的打分（不改变任何状态），
// 用于调参时回答"这条请求为什么会被路由到 X"。
func (s *Scheduler) Explain(route *backend.Route, policyName string, req *Request) []ScoreDetail {
	if policyName == "" {
		policyName = route.PolicyName
	}
	if policyName == "" {
		policyName = config.DefaultPolicyName
	}
	p := s.pol.Get(policyName)
	ratios := s.prefixMatches(req)
	now := time.Now()
	pool := route.Pool()
	out := make([]ScoreDetail, 0, len(pool))
	for _, b := range pool {
		snap := b.Snapshot()
		d := ScoreDetail{
			Backend: b.ID, Available: b.Available(now), PrefixMatch: ratios[b.ID],
			Inflight: b.Inflight(), Running: snap.Running, Waiting: snap.Waiting, KVUsage: snap.KVUsage,
		}
		if p == nil {
			d.Err = fmt.Sprintf("策略 %q 不存在", policyName)
		} else {
			pass, score, err := p.Eval(s.BuildEnv(req, b, ratios[b.ID]))
			d.Pass, d.Score = pass, score
			if err != nil {
				d.Err = err.Error()
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out
}
