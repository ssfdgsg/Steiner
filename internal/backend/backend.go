// Package backend 定义推理后端实例、注册表、模型路由与健康检查。
// Backend 上的运行态（在途请求数、指标快照、熔断状态）全部使用原子操作，
// 调度热路径读取时无锁。
package backend

import (
	"fmt"
	"math"
	"net/url"
	"sync/atomic"
	"time"

	"ai-gateway/internal/config"
)

// EngineType 推理引擎类型。
type EngineType string

// 支持的四种引擎。omni 变体与基础引擎共用指标与 PD 协议。
const (
	EngineVLLM       EngineType = "vllm"
	EngineVLLMOmni   EngineType = "vllm_omni"
	EngineSGLang     EngineType = "sglang"
	EngineSGLangOmni EngineType = "sglang_omni"
)

// Family 返回引擎家族（vllm / sglang），决定指标名映射与 PD 转发协议。
func (e EngineType) Family() string {
	switch e {
	case EngineVLLM, EngineVLLMOmni:
		return "vllm"
	case EngineSGLang, EngineSGLangOmni:
		return "sglang"
	}
	return string(e)
}

// Snapshot 为一次 /metrics 采集后的归一化视图，调度表达式直接引用其中字段。
type Snapshot struct {
	Time time.Time `json:"time"`
	// Running 引擎正在执行的请求数。
	Running float64 `json:"running"`
	// Waiting 引擎侧排队请求数。
	Waiting float64 `json:"waiting"`
	// KVUsage KV cache 使用率（0~1）。
	KVUsage float64 `json:"kv_usage"`
	// HitRate 前缀缓存命中率（0~1）。
	HitRate float64 `json:"hit_rate"`
	// GenTokPerSec 生成吞吐（token/s）。
	GenTokPerSec float64 `json:"gen_tok_per_sec"`
	// Raw 全部原始指标（同名多序列求和），counter 另有 "rate:<名>" 的速率派生值。
	Raw map[string]float64 `json:"-"`
	// Err 最近一次采集错误，空串表示成功。
	Err string `json:"err,omitempty"`
}

// Backend 单个推理后端实例。
type Backend struct {
	ID             string
	URL            *url.URL
	Engine         EngineType
	Weight         float64
	MaxConcurrency int64
	MetricsPath    string
	HealthPath     string
	BootstrapPort  int
	Labels         map[string]string

	inflight         atomic.Int64
	consecutiveFails atomic.Int32
	cordoned         atomic.Bool
	ejectedUntil     atomic.Int64 // unix 纳秒，晚于当前时间表示被动摘除中
	healthy          atomic.Bool
	snap             atomic.Pointer[Snapshot]
	promVars         atomic.Pointer[map[string]float64]
	// ttftEWMA 网关实测首字节时延的指数滑动均值（秒，Float64bits 编码）。
	// 引擎无关的时延反馈信号，供 latency_first 等策略表达式使用。
	ttftEWMA atomic.Uint64
}

// New 由配置构造后端实例。初始视为健康，由健康检查器快速纠偏，
// 避免网关启动后到首次探测之间完全不可用。
func New(cfg config.BackendConfig) (*Backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("后端 %s 的 url %q 非法", cfg.ID, cfg.URL)
	}
	b := &Backend{
		ID:             cfg.ID,
		URL:            u,
		Engine:         EngineType(cfg.Engine),
		Weight:         normalizeWeight(cfg.Weight),
		MaxConcurrency: int64(cfg.MaxConcurrency),
		MetricsPath:    cfg.MetricsPath,
		HealthPath:     cfg.HealthPath,
		BootstrapPort:  cfg.BootstrapPort,
		Labels:         cfg.Labels,
	}
	b.healthy.Store(true)
	return b, nil
}

// normalizeWeight 钳制静态权重：<=0 一律视为 1，保证加权调度（weighted_random）
// 的权重和恒 >0、每个后端至少可被加权选中。与 config.ApplyDefaults 的后端
// 默认权重 1 语义一致；注册/更新路径若未先经 ApplyDefaults 也由此兜底。
func normalizeWeight(w float64) float64 {
	if w <= 0 {
		return 1
	}
	return w
}

// Snapshot 返回最近一次指标快照；尚未采集时返回零值快照，避免调用方判空。
func (b *Backend) Snapshot() *Snapshot {
	if s := b.snap.Load(); s != nil {
		return s
	}
	return &Snapshot{Raw: map[string]float64{}}
}

// SetSnapshot 原子替换指标快照。
func (b *Backend) SetSnapshot(s *Snapshot) { b.snap.Store(s) }

// PromVars 返回外部 Prometheus 注入的表达式变量。
func (b *Backend) PromVars() map[string]float64 {
	if m := b.promVars.Load(); m != nil {
		return *m
	}
	return map[string]float64{}
}

// SetPromVars 原子替换 PromQL 变量表。
func (b *Backend) SetPromVars(m map[string]float64) { b.promVars.Store(&m) }

// Inflight 网关侧在途请求数。
func (b *Backend) Inflight() int64 { return b.inflight.Load() }

// TryAcquire 占用一个并发额度；超过 MaxConcurrency 时失败。0 表示不限。
func (b *Backend) TryAcquire() bool {
	n := b.inflight.Add(1)
	if b.MaxConcurrency > 0 && n > b.MaxConcurrency {
		b.inflight.Add(-1)
		return false
	}
	return true
}

// Release 释放一个并发额度。
func (b *Backend) Release() { b.inflight.Add(-1) }

// Available 是否可参与调度：健康、未被人工隔离且不在被动摘除冷却期内。
func (b *Backend) Available(now time.Time) bool {
	return b.healthy.Load() && !b.cordoned.Load() && now.UnixNano() >= b.ejectedUntil.Load()
}

// Cordon 人工隔离/恢复（运维维护用）。
func (b *Backend) Cordon(on bool) { b.cordoned.Store(on) }

// Cordoned 是否处于人工隔离。
func (b *Backend) Cordoned() bool { return b.cordoned.Load() }

// SetHealthy 由主动健康检查器写入。
func (b *Backend) SetHealthy(ok bool) { b.healthy.Store(ok) }

// Healthy 主动健康检查结果。
func (b *Backend) Healthy() bool { return b.healthy.Load() }

// Ejected 是否处于被动摘除冷却期。
func (b *Backend) Ejected(now time.Time) bool { return now.UnixNano() < b.ejectedUntil.Load() }

// MarkFailure 记录一次转发失败；连续失败达到阈值后进入冷却期（被动摘除）。
func (b *Backend) MarkFailure(threshold int32, cooldown time.Duration) {
	if b.consecutiveFails.Add(1) >= threshold {
		b.ejectedUntil.Store(time.Now().Add(cooldown).UnixNano())
		b.consecutiveFails.Store(0)
	}
}

// MarkSuccess 记录一次转发成功，清零连续失败计数。
func (b *Backend) MarkSuccess() { b.consecutiveFails.Store(0) }

// ttftAlpha TTFT 指数滑动均值的平滑系数：0.2 ≈ 最近 ~10 个样本主导，
// 既能跟上负载变化又不被单次毛刺带偏。
const ttftAlpha = 0.2

// ObserveTTFT 上报一次网关实测首字节时延（秒），更新指数滑动均值。
// CAS 循环保证并发上报不丢样本。
func (b *Backend) ObserveTTFT(seconds float64) {
	if seconds < 0 {
		return
	}
	for {
		old := b.ttftEWMA.Load()
		var next float64
		if old == 0 {
			next = seconds // 首个样本直接采纳
		} else {
			next = ttftAlpha*seconds + (1-ttftAlpha)*math.Float64frombits(old)
		}
		if b.ttftEWMA.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// TTFTEWMA 返回首字节时延滑动均值（秒）；尚无样本返回 0。
func (b *Backend) TTFTEWMA() float64 {
	return math.Float64frombits(b.ttftEWMA.Load())
}
