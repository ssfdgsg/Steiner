// Package metrics 提供三类指标能力：
//  1. Scraper —— 直采各后端 /metrics 文本并归一化为 backend.Snapshot；
//  2. PromCollector —— 查询外部 Prometheus，把 PromQL 结果注入表达式变量；
//  3. Gateway —— 网关自身指标的注册与导出。
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"ai-gateway/internal/backend"
)

// Scraper 周期性抓取后端 /metrics 并写入快照。
type Scraper struct {
	backends func() []*backend.Backend
	interval time.Duration
	client   *http.Client

	// prev 上一轮 counter 值与时间，用于计算速率（rate:<指标名>）。
	mu       sync.Mutex
	prev     map[string]map[string]float64
	prevTime map[string]time.Time
}

// NewScraper 构造抓取器；backends 为活视图函数（通常传 Registry.All），
// 动态注册的后端在下一个抓取节拍自动纳入。
func NewScraper(backends func() []*backend.Backend, interval, timeout time.Duration) *Scraper {
	return &Scraper{
		backends: backends,
		interval: interval,
		client:   &http.Client{Timeout: timeout},
		prev:     map[string]map[string]float64{},
		prevTime: map[string]time.Time{},
	}
}

// Run 统一节拍抓取：每个周期对当前全部后端并发抓取一轮（各后端互为独立
// 主机，无同步风暴问题），阻塞到 ctx 取消。启动时立即抓取一轮。
func (s *Scraper) Run(ctx context.Context) {
	s.scrapeAll(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scrapeAll(ctx)
		}
	}
}

// scrapeAll 并发抓取当前全部后端，等待整轮完成（受 client 超时约束），
// 随后清理已被动态摘除后端的速率基线，避免长期运行下基线表无界增长。
func (s *Scraper) scrapeAll(ctx context.Context) {
	backends := s.backends()
	var wg sync.WaitGroup
	for _, b := range backends {
		b := b
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ScrapeOnce(ctx, b)
		}()
	}
	wg.Wait()

	alive := make(map[string]bool, len(backends))
	for _, b := range backends {
		alive[b.ID] = true
	}
	s.mu.Lock()
	for id := range s.prev {
		if !alive[id] {
			delete(s.prev, id)
			delete(s.prevTime, id)
		}
	}
	s.mu.Unlock()
}

// ScrapeOnce 抓取单个后端一次并更新快照（导出以便测试直接驱动）。
func (s *Scraper) ScrapeOnce(ctx context.Context, b *backend.Backend) {
	snap, err := s.scrape(ctx, b)
	if err != nil {
		// 采集失败保留旧数据字段，仅追加错误标记，避免调度突然失去所有信息。
		old := b.Snapshot()
		snap = &backend.Snapshot{
			Time: time.Now(), Running: old.Running, Waiting: old.Waiting,
			KVUsage: old.KVUsage, HitRate: old.HitRate, GenTokPerSec: old.GenTokPerSec,
			Raw: old.Raw, Err: err.Error(),
		}
		slog.Debug("指标采集失败", "backend", b.ID, "err", err)
	}
	b.SetSnapshot(snap)
}

func (s *Scraper) scrape(ctx context.Context, b *backend.Backend) (*backend.Snapshot, error) {
	u := *b.URL
	u.Path = b.MetricsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics 端点返回 %d", resp.StatusCode)
	}

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析 metrics 文本失败: %w", err)
	}

	now := time.Now()
	raw := map[string]float64{}
	counters := map[string]float64{}
	for name, mf := range families {
		var sum float64
		for _, m := range mf.GetMetric() {
			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				sum += m.GetGauge().GetValue()
			case dto.MetricType_COUNTER:
				sum += m.GetCounter().GetValue()
			case dto.MetricType_UNTYPED:
				sum += m.GetUntyped().GetValue()
			case dto.MetricType_HISTOGRAM:
				// 直方图取样本总数，便于表达式引用请求量级。
				sum += float64(m.GetHistogram().GetSampleCount())
			default:
				continue
			}
		}
		raw[name] = sum
		if mf.GetType() == dto.MetricType_COUNTER {
			counters[name] = sum
		}
	}

	// counter 速率派生：rate:<指标名> = (本轮值 - 上轮值) / 间隔秒数。
	s.mu.Lock()
	prev := s.prev[b.ID]
	prevTime := s.prevTime[b.ID]
	s.prev[b.ID] = counters
	s.prevTime[b.ID] = now
	s.mu.Unlock()
	if prev != nil {
		dt := now.Sub(prevTime).Seconds()
		if dt > 0 {
			for name, v := range counters {
				if pv, ok := prev[name]; ok && v >= pv {
					raw["rate:"+name] = (v - pv) / dt
				}
			}
		}
	}

	ad := adapters[b.Engine.Family()]
	snap := &backend.Snapshot{Time: now, Raw: raw}
	snap.Running, _ = pick(raw, ad.running)
	snap.Waiting, _ = pick(raw, ad.waiting)
	snap.KVUsage, _ = pick(raw, ad.kvUsage)

	if v, ok := pick(raw, ad.hitRate); ok {
		snap.HitRate = v
	} else if len(ad.hitQueriesCounter) > 0 {
		// 新版引擎只暴露 queries/hits 累计 counter，用速率比推导瞬时命中率。
		q, qok := pickRate(raw, ad.hitQueriesCounter)
		h, hok := pickRate(raw, ad.hitHitsCounter)
		if qok && hok && q > 0 {
			snap.HitRate = h / q
		}
	}

	if v, ok := pick(raw, ad.genThroughputGauge); ok {
		snap.GenTokPerSec = v
	} else if v, ok := pickRate(raw, ad.genTokensCounter); ok {
		snap.GenTokPerSec = v
	}
	return snap, nil
}

// pickRate 取候选 counter 的速率派生值。
func pickRate(raw map[string]float64, candidates []string) (float64, bool) {
	for _, name := range candidates {
		if v, ok := raw["rate:"+name]; ok {
			return v, true
		}
	}
	return 0, false
}
