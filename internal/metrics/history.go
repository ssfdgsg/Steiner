// 管理面时序历史：进程内环形缓冲，周期从自身指标与运行态探针采样，
// 供控制台绘制吞吐/时延/容量趋势图（GET /admin/stats/history）。
//
// 定位：仅覆盖"最近数小时、单实例、控制台自助排障"场景。长周期、跨实例、
// 高保真的时序仍以 Prometheus 为事实来源，本缓冲不做持久化，重启即清空。
package metrics

import (
	"context"
	"sync"
	"time"
)

// RuntimeSnapshot 采样时刻的运行态（由装配期注入的探针提供）。
// 这些数值不在网关自身指标里（排队深度为抓取时实时读、后端快照来自直采），
// 因此通过探针一并采集，保证同一采样点的各维度时间对齐。
type RuntimeSnapshot struct {
	QueueDepth      int64
	BackendsHealthy int
	BackendsTotal   int
	KVBytes         int64
	KVNodes         int64
	KVUsage         float64
	HitRate         float64
	GenTokPerSec    float64
}

// RuntimeProbe 运行态探针。
type RuntimeProbe func() RuntimeSnapshot

// Sample 一个采样点。速率类字段为相邻两次采样的差分值，首个采样点仅作基线，
// 不进入缓冲（避免把进程启动以来的累计值误当作瞬时速率）。
type Sample struct {
	Time            time.Time `json:"time"`
	RPS             float64   `json:"rps"`
	ErrorRate       float64   `json:"error_rate"`
	LatencyAvgMs    float64   `json:"latency_avg_ms"`
	LatencyP95Ms    float64   `json:"latency_p95_ms"`
	TTFTP95Ms       float64   `json:"ttft_p95_ms"`
	QueueDepth      int64     `json:"queue_depth"`
	BackendsHealthy int       `json:"backends_healthy"`
	BackendsTotal   int       `json:"backends_total"`
	KVBytes         int64     `json:"kv_bytes"`
	KVNodes         int64     `json:"kv_nodes"`
	KVUsage         float64   `json:"kv_usage"`
	HitRate         float64   `json:"hit_rate"`
	GenTokPerSec    float64   `json:"gen_tok_per_sec"`
}

// History 采样缓冲。
type History struct {
	gw       *Gateway
	interval time.Duration
	capacity int

	mu    sync.RWMutex
	buf   []Sample // 环形缓冲，按时间升序（满后从头部丢弃）
	probe RuntimeProbe
	prev  counterState
}

// counterState 上一次采样的累计量，用于差分。
type counterState struct {
	set      bool
	at       time.Time
	requests float64
	errors   float64
	durSum   float64
	durCount float64
}

// NewHistory 构造采样缓冲。interval <= 0 或 capacity <= 0 时使用默认值
// （15s / 1440 点，约 6 小时）。
func NewHistory(gw *Gateway, interval time.Duration, capacity int) *History {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if capacity <= 0 {
		capacity = 1440
	}
	return &History{gw: gw, interval: interval, capacity: capacity, buf: make([]Sample, 0, capacity)}
}

// SetProbe 注入运行态探针（装配期调用，可不设置）。
func (h *History) SetProbe(p RuntimeProbe) {
	h.mu.Lock()
	h.probe = p
	h.mu.Unlock()
}

// Interval 采样间隔。
func (h *History) Interval() time.Duration { return h.interval }

// Capacity 缓冲容量（采样点数）。
func (h *History) Capacity() int { return h.capacity }

// Run 周期采样直到 ctx 取消。启动时立刻采一次作为差分基线。
func (h *History) Run(ctx context.Context) {
	h.sample(time.Now())
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.sample(now)
		}
	}
}

// sample 采集一个点。Gather 失败时跳过本轮（保留上次基线，下轮差分跨两个间隔仍正确）。
func (h *History) sample(now time.Time) {
	agg, err := h.gw.Aggregate()
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	cur := counterState{
		set: true, at: now,
		requests: agg.RequestsTotal,
		errors:   agg.ErrorsTotal,
		// Aggregate 只暴露均值，用 均值*次数 还原直方图 sum 以做区间差分。
		durSum:   agg.Latency.AvgMs * agg.Latency.Count,
		durCount: agg.Latency.Count,
	}
	prev := h.prev
	h.prev = cur

	if !prev.set {
		return
	}
	dt := now.Sub(prev.at).Seconds()
	if dt <= 0 {
		return
	}
	s := Sample{
		Time:         now,
		LatencyP95Ms: agg.Latency.P95Ms,
		TTFTP95Ms:    agg.TTFT.P95Ms,
	}
	if dReq := agg.RequestsTotal - prev.requests; dReq > 0 {
		s.RPS = dReq / dt
		s.ErrorRate = (agg.ErrorsTotal - prev.errors) / dReq
	}
	if dCount := cur.durCount - prev.durCount; dCount > 0 {
		s.LatencyAvgMs = (cur.durSum - prev.durSum) / dCount
	}
	if h.probe != nil {
		rt := h.probe()
		s.QueueDepth = rt.QueueDepth
		s.BackendsHealthy = rt.BackendsHealthy
		s.BackendsTotal = rt.BackendsTotal
		s.KVBytes = rt.KVBytes
		s.KVNodes = rt.KVNodes
		s.KVUsage = rt.KVUsage
		s.HitRate = rt.HitRate
		s.GenTokPerSec = rt.GenTokPerSec
	}
	if len(h.buf) == h.capacity {
		copy(h.buf, h.buf[1:])
		h.buf[h.capacity-1] = s
		return
	}
	h.buf = append(h.buf, s)
}

// Samples 返回 since 之后的采样点（时间升序副本）。since 为零值表示全部。
func (h *History) Samples(since time.Time) []Sample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Sample, 0, len(h.buf))
	for _, s := range h.buf {
		if since.IsZero() || s.Time.After(since) {
			out = append(out, s)
		}
	}
	return out
}
