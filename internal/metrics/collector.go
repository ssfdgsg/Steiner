// BackendCollector 把各后端的归一化指标快照透出为网关自身指标：
// Prometheus 只需抓取网关一个端点即可获得全集群推理引擎的核心负载视图
// （也是告警/扩缩容表达式所见数据的对外佐证）。
//
// 实现为自定义 prometheus.Collector：抓取时实时读原子快照（无锁），
// 每次全量生成样本，后端摘除后不会留下陈旧序列。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"ai-gateway/internal/backend"
)

// 各透出指标的描述符。
var (
	descBackendInfo = prometheus.NewDesc(
		"gateway_backend_info",
		"后端静态信息，值恒为 1（backend/engine/url 经标签暴露）",
		[]string{"backend", "engine", "url"}, nil)
	descBackendRunning = prometheus.NewDesc(
		"gateway_backend_running_requests",
		"后端引擎正在执行的请求数（直采归一化）",
		[]string{"backend"}, nil)
	descBackendWaiting = prometheus.NewDesc(
		"gateway_backend_waiting_requests",
		"后端引擎排队中的请求数（直采归一化）",
		[]string{"backend"}, nil)
	descBackendKVUsage = prometheus.NewDesc(
		"gateway_backend_kv_cache_usage",
		"后端 KV cache 使用率（0~1，直采归一化）",
		[]string{"backend"}, nil)
	descBackendHitRate = prometheus.NewDesc(
		"gateway_backend_prefix_hit_rate",
		"后端前缀缓存命中率（0~1，直采归一化）",
		[]string{"backend"}, nil)
	descBackendGenTPS = prometheus.NewDesc(
		"gateway_backend_gen_tokens_per_second",
		"后端生成吞吐（token/s，直采归一化）",
		[]string{"backend"}, nil)
	descBackendScrapeUp = prometheus.NewDesc(
		"gateway_backend_scrape_up",
		"最近一次直采后端 /metrics 是否成功（1 成功 / 0 失败或尚未采集）",
		[]string{"backend"}, nil)
	descBackendHealthy = prometheus.NewDesc(
		"gateway_backend_healthy",
		"后端健康状态（1 健康 / 0 不健康）",
		[]string{"backend"}, nil)
	descBackendGWInflight = prometheus.NewDesc(
		"gateway_backend_inflight",
		"网关侧在途请求数",
		[]string{"backend"}, nil)
	descSplitRequests = prometheus.NewDesc(
		"gateway_split_requests_total",
		"权重分池（金丝雀）各子池被选中次数",
		[]string{"model", "split"}, nil)
	descQueueDepth = prometheus.NewDesc(
		"gateway_queue_depth",
		"容量排队当前深度（按模型路由分列，总深度用 sum() 聚合）",
		[]string{"model"}, nil)
)

// BackendCollector 后端指标透出采集器。
type BackendCollector struct {
	backends func() []*backend.Backend
}

// NewBackendCollector 构造采集器；backends 为活视图函数（通常传 Registry.All），
// 每次抓取实时取当前后端集合，动态增删后端即刻反映。
func NewBackendCollector(backends func() []*backend.Backend) *BackendCollector {
	return &BackendCollector{backends: backends}
}

// Describe 实现 prometheus.Collector。
func (c *BackendCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descBackendInfo
	ch <- descBackendRunning
	ch <- descBackendWaiting
	ch <- descBackendKVUsage
	ch <- descBackendHitRate
	ch <- descBackendGenTPS
	ch <- descBackendScrapeUp
	ch <- descBackendHealthy
	ch <- descBackendGWInflight
}

// Collect 实现 prometheus.Collector：读各后端最新快照并生成样本。
func (c *BackendCollector) Collect(ch chan<- prometheus.Metric) {
	for _, b := range c.backends() {
		snap := b.Snapshot()
		ch <- prometheus.MustNewConstMetric(descBackendInfo, prometheus.GaugeValue, 1,
			b.ID, string(b.Engine), b.URL.String())
		ch <- prometheus.MustNewConstMetric(descBackendRunning, prometheus.GaugeValue, snap.Running, b.ID)
		ch <- prometheus.MustNewConstMetric(descBackendWaiting, prometheus.GaugeValue, snap.Waiting, b.ID)
		ch <- prometheus.MustNewConstMetric(descBackendKVUsage, prometheus.GaugeValue, snap.KVUsage, b.ID)
		ch <- prometheus.MustNewConstMetric(descBackendHitRate, prometheus.GaugeValue, snap.HitRate, b.ID)
		ch <- prometheus.MustNewConstMetric(descBackendGenTPS, prometheus.GaugeValue, snap.GenTokPerSec, b.ID)
		up := 0.0
		// 采集成功的判定：至少成功采集过一次（Time 非零）且最近一次无错误。
		if !snap.Time.IsZero() && snap.Err == "" {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(descBackendScrapeUp, prometheus.GaugeValue, up, b.ID)
		healthy := 0.0
		if b.Healthy() {
			healthy = 1
		}
		ch <- prometheus.MustNewConstMetric(descBackendHealthy, prometheus.GaugeValue, healthy, b.ID)
		ch <- prometheus.MustNewConstMetric(descBackendGWInflight, prometheus.GaugeValue, float64(b.Inflight()), b.ID)
	}
}

// DepthReporter 提供按类别的排队深度快照（由 internal/queue.Queue 实现）。
// 用小接口解耦，metrics 包不依赖 queue 包。
type DepthReporter interface {
	Depths() map[string]int64
}

// QueueCollector 抓取时实时读排队深度，按模型路由分列暴露。
// 只在被抓取瞬间生成样本：队列清空后序列自然消失，无陈旧标签残留。
type QueueCollector struct {
	q DepthReporter
}

// NewQueueCollector 构造排队深度采集器。
func NewQueueCollector(q DepthReporter) *QueueCollector {
	return &QueueCollector{q: q}
}

// Describe 实现 prometheus.Collector。
func (c *QueueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descQueueDepth
}

// Collect 实现 prometheus.Collector。
func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	for model, depth := range c.q.Depths() {
		ch <- prometheus.MustNewConstMetric(descQueueDepth, prometheus.GaugeValue, float64(depth), model)
	}
}

// RouteCollector 透出模型路由级的运行态指标（目前为分池分流计数）。
// 与 BackendCollector 同为只读 Collector：抓取时实时读原子计数。
type RouteCollector struct {
	routes map[string]*backend.Route
}

// NewRouteCollector 构造路由指标采集器。
func NewRouteCollector(routes map[string]*backend.Route) *RouteCollector {
	return &RouteCollector{routes: routes}
}

// Describe 实现 prometheus.Collector。
func (c *RouteCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descSplitRequests
}

// Collect 实现 prometheus.Collector。
func (c *RouteCollector) Collect(ch chan<- prometheus.Metric) {
	for name, rt := range c.routes {
		for _, sp := range rt.Splits {
			ch <- prometheus.MustNewConstMetric(descSplitRequests, prometheus.CounterValue,
				float64(sp.Hits.Load()), name, sp.Name)
		}
	}
}
