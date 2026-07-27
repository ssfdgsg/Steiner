// 网关自身指标：请求量、时延、首字节时延（TTFT）、重试、限流、
// 后端健康/在途、KV 前缀树规模、PD 链路在途传输数等。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Gateway 网关自身指标集合，全部注册在独立 Registry 上，
// 由 /metrics 端点以 Prometheus 文本格式导出。
type Gateway struct {
	Registry *prometheus.Registry

	ReqTotal    *prometheus.CounterVec   // 请求总数 {backend, model, code}
	ReqDuration *prometheus.HistogramVec // 整请求时延 {backend, model}
	TTFT        *prometheus.HistogramVec // 首字节时延 {backend, model}
	Retries     prometheus.Counter       // 换后端重试次数
	RateLimited *prometheus.CounterVec   // 被限流请求数 {model}
	PickErrors  *prometheus.CounterVec   // 选路失败数 {model, reason}

	// 后端健康/在途经 BackendCollector 抓取时实时透出（动态摘除后端不留陈旧序列）。
	KVTreeBytes    prometheus.Gauge     // 前缀树占用字节数
	KVTreeNodes    prometheus.Gauge     // 前缀树节点数
	PDLinkInflight *prometheus.GaugeVec // PD 链路在途 KV 传输数 {prefill, decode}

	AlertsFiring     *prometheus.GaugeVec   // 每条规则 firing 实例数 {rule}
	WebhookSent      *prometheus.CounterVec // webhook 投递结果 {target, outcome}
	AutoscaleDesired *prometheus.GaugeVec   // 建议副本数 {model}，可作 HPA/KEDA 外部指标

	PromptTokens     *prometheus.CounterVec   // 上游用量：prompt token 数 {backend, model}
	CompletionTokens *prometheus.CounterVec   // 上游用量：completion token 数 {backend, model}
	UpstreamErrors   *prometheus.CounterVec   // 上游错误分类 {backend, kind}
	PickDuration     *prometheus.HistogramVec // 单次选路耗时 {strategy}
	BuildInfo        *prometheus.GaugeVec     // 构建信息 {version}，恒为 1

	ClusterLeader      prometheus.Gauge       // 本实例是否为集群 leader（1/0，未启用集群恒为 0）
	ClusterRedisErrors *prometheus.CounterVec // 集群协调 Redis 操作失败数 {op}
}

// NewGateway 构造并注册全部指标。
func NewGateway() *Gateway {
	reg := prometheus.NewRegistry()
	g := &Gateway{
		Registry: reg,
		ReqTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "网关转发请求总数",
		}, []string{"backend", "model", "code"}),
		ReqDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "整请求时延（秒）",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 14),
		}, []string{"backend", "model"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_time_to_first_byte_seconds",
			Help:    "后端首字节时延（秒），流式请求即 TTFT",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 14),
		}, []string{"backend", "model"}),
		Retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_retries_total",
			Help: "换后端重试总次数",
		}),
		RateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limited_total",
			Help: "因模型级限流被拒绝的请求数",
		}, []string{"model"}),
		PickErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_pick_errors_total",
			Help: "选路失败次数",
		}, []string{"model", "reason"}),
		KVTreeBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_kvtree_bytes",
			Help: "KV 前缀树当前占用字节数",
		}),
		KVTreeNodes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_kvtree_nodes",
			Help: "KV 前缀树当前节点数",
		}),
		PDLinkInflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_pd_link_inflight",
			Help: "PD 分离链路在途 KV 传输数",
		}, []string{"prefill", "decode"}),
		AlertsFiring: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_alerts_firing",
			Help: "每条告警规则当前 firing 的实例数",
		}, []string{"rule"}),
		WebhookSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_webhook_sent_total",
			Help: "webhook 投递结果计数（outcome: ok/failed/dropped）",
		}, []string{"target", "outcome"}),
		AutoscaleDesired: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_autoscale_desired_replicas",
			Help: "自动扩缩容建议的期望副本数，可作为 HPA/KEDA 外部指标源",
		}, []string{"model"}),
		PromptTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_prompt_tokens_total",
			Help: "上游响应 usage 中的 prompt token 总数（流式需后端在末块携带 usage）",
		}, []string{"backend", "model"}),
		CompletionTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_completion_tokens_total",
			Help: "上游响应 usage 中的 completion token 总数",
		}, []string{"backend", "model"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_errors_total",
			Help: "上游错误分类计数（kind: connect/bad_status/stream）",
		}, []string{"backend", "kind"}),
		PickDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_pick_duration_seconds",
			Help:    "单次调度选路耗时（秒）",
			Buckets: prometheus.ExponentialBuckets(0.000005, 4, 10), // 5µs ~ 1.3s
		}, []string{"strategy"}),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_build_info",
			Help: "构建信息，值恒为 1，版本经 version 标签暴露",
		}, []string{"version"}),
		ClusterLeader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_cluster_leader",
			Help: "本实例是否持有集群 leader 租约（1/0），未启用集群恒为 0",
		}),
		ClusterRedisErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cluster_redis_errors_total",
			Help: "集群协调 Redis 操作失败数（op: heartbeat/leader/members/ratelimit/session/policy_publish）",
		}, []string{"op"}),
	}
	reg.MustRegister(
		g.ReqTotal, g.ReqDuration, g.TTFT, g.Retries, g.RateLimited, g.PickErrors,
		g.KVTreeBytes, g.KVTreeNodes, g.PDLinkInflight,
		g.AlertsFiring, g.WebhookSent, g.AutoscaleDesired,
		g.PromptTokens, g.CompletionTokens, g.UpstreamErrors, g.PickDuration, g.BuildInfo,
		g.ClusterLeader, g.ClusterRedisErrors,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return g
}
