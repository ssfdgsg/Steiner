// Package config 负责网关配置的加载、默认值填充与静态校验。
// 配置文件使用 YAML，所有引用关系（模型路由 -> 后端 / PD 组 / 策略）在加载期校验，
// 保证运行期不会出现悬空引用。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	// 仅引用预设方案表（policy 包无任何内部依赖，方向无环）；
	// policy 包禁止反向 import config。
	"ai-gateway/internal/policy"
)

// Duration 支持 "500ms"、"10s" 等字符串写法，也兼容纯数字（按秒解释）。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.v3 的自定义解码。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		if v, err := time.ParseDuration(s); err == nil {
			*d = Duration(v)
			return nil
		}
		// 纯数字标量也会被解码为字符串（如 "3"），回退按秒解释。
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
			*d = Duration(time.Duration(f * float64(time.Second)))
			return nil
		}
		return fmt.Errorf("非法时长 %q（支持 \"500ms\"/\"10s\" 或纯数字秒）", s)
	}
	return fmt.Errorf("时长字段格式错误: %s", value.Value)
}

// D 返回标准库时长。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config 为网关全量配置根节点。
type Config struct {
	Server     ServerConfig            `yaml:"server"`
	Metrics    MetricsConfig           `yaml:"metrics"`
	Prometheus PrometheusConfig        `yaml:"prometheus"`
	KVCache    KVCacheConfig           `yaml:"kv_cache"`
	Policies   map[string]PolicyConfig `yaml:"policies"`
	Backends   []BackendConfig         `yaml:"backends"`
	Models     []ModelRoute            `yaml:"models"`
	PDGroups   []PDGroupConfig         `yaml:"pd_groups"`
	Alerting   AlertingConfig          `yaml:"alerting"`
	Autoscale  AutoscaleConfig         `yaml:"autoscale"`
	Session    SessionConfig           `yaml:"session"`
	Queue      QueueConfig             `yaml:"queue"`
	Cluster    ClusterConfig           `yaml:"cluster"`
	Tracing    TracingConfig           `yaml:"tracing"`
	Rollouts   []RolloutConfig         `yaml:"rollouts"`
	Store      StoreConfig             `yaml:"store"`
}

// StoreConfig 动态配置持久层：admin 运行期变更（后端增删、策略热更新）
// 落库，重启后自动加载并覆盖 YAML 同名项（DB 是运行期变更的权威来源，
// YAML 是初始基线）。支持 PostgreSQL 与 MySQL。
type StoreConfig struct {
	Enabled bool `yaml:"enabled"`
	// Driver 数据库类型：postgres | mysql。
	Driver string `yaml:"driver"`
	// DSN 连接串。postgres 形如 postgres://user:pass@host:5432/db，
	// mysql 形如 user:pass@tcp(host:3306)/db?parseTime=true。
	DSN string `yaml:"dsn"`
	// MaxOpenConns 连接池上限；持久层只承载低频管理操作，默认 4。
	MaxOpenConns int `yaml:"max_open_conns"`
	// OpTimeout 单次数据库操作超时。
	OpTimeout Duration `yaml:"op_timeout"`
	// ReconcileInterval 动态配置对账周期（以 DB 为事实来源收敛错过的
	// 集群广播），默认 30s。
	ReconcileInterval Duration `yaml:"reconcile_interval"`
}

// TracingConfig 分布式追踪（OpenTelemetry）配置。启用后每个请求产生一条
// span 链：根 span（请求全程）→ 排队等待 → 每次选路 → 每次转发（含 TTFT 事件），
// 经 OTLP/HTTP 导出到 Collector/Jaeger/Tempo 等；traceparent 会注入上游请求头，
// 后端引擎若启用 OTel 可续接同一条 trace。
type TracingConfig struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint OTLP/HTTP 接收端地址（host:port，不含路径），enabled 时必填。
	Endpoint string `yaml:"endpoint"`
	// Insecure 使用明文 HTTP（默认 true，集群内 Collector 常见形态）。
	Insecure *bool `yaml:"insecure"`
	// SampleRatio 采样率 0~1，默认 1（全采样；父 trace 已采样则跟随父决策）。
	SampleRatio *float64 `yaml:"sample_ratio"`
	// ServiceName 资源属性 service.name，默认 "ai-gateway"。
	ServiceName string `yaml:"service_name"`
	// Headers 导出请求附加头（如认证 token）。
	Headers map[string]string `yaml:"headers"`
}

// ClusterConfig 多实例水平部署配置：以 Redis 为协调层，提供分布式限流
// （集群级配额）、会话粘性共享、策略热更新广播、告警/扩缩容单主执行
// （leader 选举）与集群成员视图。单实例部署保持 enabled=false 即可，
// 行为与既有单机模式完全一致。
type ClusterConfig struct {
	Enabled bool `yaml:"enabled"`
	// RedisAddr 单机 Redis 地址（host:port）；与 RedisAddrs 二选一。
	RedisAddr string `yaml:"redis_addr"`
	// RedisAddrs 多地址：配合 RedisMasterName 为 Sentinel 地址列表（HA 推荐），
	// 否则视为 Redis Cluster 节点列表（协调键自动加 hash tag 落同一 slot）。
	RedisAddrs []string `yaml:"redis_addrs"`
	// RedisMasterName Sentinel 监控的主节点名（如 "mymaster"）；非空即启用
	// Sentinel 故障转移模式。
	RedisMasterName string `yaml:"redis_master_name"`
	RedisUsername   string `yaml:"redis_username"`
	RedisPassword   string `yaml:"redis_password"`
	RedisDB         int    `yaml:"redis_db"`
	// KeyPrefix 全部键与频道的前缀，同一 Redis 承载多套网关集群时用于隔离。
	KeyPrefix string `yaml:"key_prefix"`
	// InstanceID 实例唯一标识，缺省为 主机名+监听地址；同一集群内不得重复。
	InstanceID string `yaml:"instance_id"`
	// HeartbeatInterval / HeartbeatTTL 实例注册心跳周期与存活 TTL，
	// TTL 内未续心跳的实例从成员视图中自动消失。
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	HeartbeatTTL      Duration `yaml:"heartbeat_ttl"`
	// LeaderTTL leader 租约时长；租约到期未续期即自动重新选举。
	LeaderTTL Duration `yaml:"leader_ttl"`
	// RateLimitMode 限流模式：
	//   distributed  集群级 GCRA 配额，全部实例共享 rate_limit_qps（缺省）；
	//   local        各实例独立限流，总放行量 = 配额 × 实例数。
	RateLimitMode string `yaml:"rate_limit_mode"`
	// SessionMode 会话粘性模式：
	//   shared  绑定表存 Redis，跨实例一致（缺省）；
	//   local   各实例独立绑定，需前置 LB 按会话键做一致性哈希。
	SessionMode string `yaml:"session_mode"`
	// OpTimeout 单次 Redis 操作超时；超时按 Redis 故障降级处理。
	OpTimeout Duration `yaml:"op_timeout"`
}

// RolloutConfig 金丝雀自动升降级：对带 splits 分池的模型路由做渐进式放量。
// 金丝雀子池权重按 steps 阶梯自动爬升，每步观察 interval 时长后用 promote_expr
// 判定是否晋级；rollback_expr 任意时刻命中即把金丝雀权重清零并终止发布。
// 判据变量为金丝雀池与稳定池（其余子池聚合）的窗口对比统计：
// canary_requests / canary_error_rate / canary_ttft_p95 / canary_ttft_avg、
// stable_* 同名四项、canary_weight / step / elapsed。
type RolloutConfig struct {
	// Model 目标模型路由名，必须配置了 splits 且包含 Canary 子池与至少一个其他子池。
	Model string `yaml:"model"`
	// Canary 金丝雀子池名。
	Canary string `yaml:"canary"`
	// Steps 放量阶梯（金丝雀权重百分比，(0,100] 严格递增，末值 100 表示全量）。
	Steps []float64 `yaml:"steps"`
	// Interval 每步观察时长，默认 2m。
	Interval Duration `yaml:"interval"`
	// PromoteExpr 晋级判据（bool，必填）：观察期满且为 true 才进入下一步。
	PromoteExpr string `yaml:"promote_expr"`
	// RollbackExpr 回滚判据（bool，必填）：任意求值周期命中立即回滚。
	RollbackExpr string `yaml:"rollback_expr"`
	// Webhooks 发布事件（started/promoted/completed/rolled_back）投递目标，
	// 引用 alerting.webhooks 中的名字。
	Webhooks []string `yaml:"webhooks"`
	// AutoStart 启动即从第一阶开始放量；false 则等待 admin reset 触发。默认 true。
	AutoStart *bool `yaml:"auto_start"`
}

// SessionConfig 会话粘性配置：同一会话（X-Session-Id 头或请求体 user 字段）
// 的多轮请求固定路由到同一后端，最大化后端 KV cache 命中；
// 与 kvcache 前缀亲和互补（粘性是确定性查表，前缀匹配是概率性收益）。
type SessionConfig struct {
	Enabled bool `yaml:"enabled"`
	// TTL 绑定关系的滑动过期时间。
	TTL Duration `yaml:"ttl"`
	// MaxEntries 绑定表容量上限，超限按最久未用淘汰（防会话键爆炸）。
	MaxEntries int `yaml:"max_entries"`
}

// QueueConfig 请求排队配置：集群瞬时无可用容量时短暂排队等待容量释放，
// 而非立即失败，削峰填谷。
type QueueConfig struct {
	Enabled bool `yaml:"enabled"`
	// MaxWait 单请求最长排队时长，超时返回 503。
	MaxWait Duration `yaml:"max_wait"`
	// MaxDepth 队列深度上限，超限立即返回 429。
	MaxDepth int `yaml:"max_depth"`
	// AdmissionExpr 容量准入表达式（bool）：请求将要排队时求值，false 直接 429
	// 快速失败，避免明显装不下的请求排到超时。可用变量 = 请求特征
	//（prompt_tokens_est/is_multimodal/image_count/...）+ 该模型路由的集群聚合
	//（avg_kv_usage/total_waiting/available_count/...），空串表示不启用。
	// 示例：'prompt_tokens_est < 32000 && (avg_kv_usage < 0.9 || prompt_tokens_est < 2000)'
	AdmissionExpr string `yaml:"admission_expr"`
}

// ServerConfig 为 HTTP 服务与转发行为配置。
type ServerConfig struct {
	// Listen 监听地址，业务、指标与管理接口共用同一端口。
	Listen string `yaml:"listen"`
	// MaxBodyBytes 请求体大小上限，防止异常大包拖垮网关。
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// Retries 转发失败（未写出任何字节前）的最大重试次数，重试会换后端。
	Retries int `yaml:"retries"`
	// UpstreamConnectTimeout 与后端建连超时。
	UpstreamConnectTimeout Duration `yaml:"upstream_connect_timeout"`
	// UpstreamResponseTimeout 等待后端响应头的超时（流式响应体不受限）。
	UpstreamResponseTimeout Duration `yaml:"upstream_response_timeout"`
	// FailureThreshold 连续失败多少次后被动摘除后端。
	FailureThreshold int `yaml:"failure_threshold"`
	// EjectCooldown 被动摘除后的冷却时长，到期自动恢复参与调度。
	EjectCooldown Duration `yaml:"eject_cooldown"`
}

// MetricsConfig 为后端 /metrics 直采配置。
type MetricsConfig struct {
	ScrapeInterval Duration `yaml:"scrape_interval"`
	ScrapeTimeout  Duration `yaml:"scrape_timeout"`
	// HealthInterval 主动健康检查周期。
	HealthInterval Duration `yaml:"health_interval"`
	HealthTimeout  Duration `yaml:"health_timeout"`
}

// PrometheusConfig 为外部 Prometheus 旁路查询配置，查询结果按标签匹配
// 注入到对应后端的表达式变量 vars 中。
type PrometheusConfig struct {
	// URL 为空表示不启用外部 Prometheus。
	URL      string        `yaml:"url"`
	Interval Duration      `yaml:"interval"`
	Timeout  Duration      `yaml:"timeout"`
	Queries  []PromQLQuery `yaml:"queries"`
}

// PromQLQuery 定义一条 PromQL 查询到表达式变量的映射。
type PromQLQuery struct {
	// Name 注入后的变量名，表达式中以 vars["name"] 引用。
	Name string `yaml:"name"`
	// Query PromQL 表达式，应返回即时向量。
	Query string `yaml:"query"`
	// BackendLabel 用该标签的值与后端 labels 中同名标签匹配，默认 "instance"。
	BackendLabel string `yaml:"backend_label"`
}

// KVCacheConfig 为前缀感知（KV cache 亲和）路由配置。
type KVCacheConfig struct {
	Enabled bool `yaml:"enabled"`
	// MaxPrefixBytes 每条请求纳入前缀树的最大字节数。
	MaxPrefixBytes int `yaml:"max_prefix_bytes"`
	// TTL 前缀归属记录的过期时间。
	TTL Duration `yaml:"ttl"`
	// PruneInterval 过期清理周期。
	PruneInterval Duration `yaml:"prune_interval"`
	// MatchThreshold cache_aware 策略走亲和路由所需的最小前缀命中率（0~1）。
	MatchThreshold float64 `yaml:"match_threshold"`
	// BalanceAbsThreshold / BalanceRelThreshold 负载失衡判定：
	// 候选后端负载 > abs 且 > rel * 最小负载时放弃亲和，回退最小负载路由。
	BalanceAbsThreshold float64 `yaml:"balance_abs_threshold"`
	BalanceRelThreshold float64 `yaml:"balance_rel_threshold"`
}

// PolicyConfig 为一条可动态编译的调度策略。
type PolicyConfig struct {
	// Preset 引用内置调度方案预设（balanced / cache_affinity / latency_first /
	// preemption_safe / throughput_first，清单见 internal/policy/presets.go），
	// 与手写 filter/score 二选一；加载期展开为预设的表达式。
	Preset string `yaml:"preset"`
	// Filter 过滤表达式，返回 bool，false 表示该后端不参与本次调度。
	Filter string `yaml:"filter"`
	// Score 打分表达式，返回数值，分数最小者胜出。
	Score string `yaml:"score"`
}

// BackendConfig 为单个推理后端实例配置。
type BackendConfig struct {
	ID     string `yaml:"id"`
	URL    string `yaml:"url"`
	Engine string `yaml:"engine"` // vllm | vllm_omni | sglang | sglang_omni
	// Weight 静态权重，表达式中以 weight 引用。
	Weight float64 `yaml:"weight"`
	// MaxConcurrency 网关侧并发上限，0 表示不限。
	MaxConcurrency int `yaml:"max_concurrency"`
	// MetricsPath / HealthPath 缺省分别为 /metrics 与 /health。
	MetricsPath string `yaml:"metrics_path"`
	HealthPath  string `yaml:"health_path"`
	// BootstrapPort sglang PD 分离场景 prefill 实例的 bootstrap 端口。
	BootstrapPort int `yaml:"bootstrap_port"`
	// Labels 自定义标签，用于 PromQL 结果匹配与表达式引用。
	Labels map[string]string `yaml:"labels"`
}

// ApplyDefaults 填充单个后端配置的缺省值。
// 除启动期配置加载外，动态注册后端（admin API / 集群广播 / 持久层加载）
// 也经此填充，保证与静态配置同一套缺省语义。
func (b *BackendConfig) ApplyDefaults() {
	if b.MetricsPath == "" {
		b.MetricsPath = "/metrics"
	}
	if b.HealthPath == "" {
		b.HealthPath = "/health"
	}
	if b.Weight <= 0 {
		b.Weight = 1
	}
	if b.BootstrapPort <= 0 {
		b.BootstrapPort = 8998
	}
	if b.Labels == nil {
		b.Labels = map[string]string{}
	}
}

// ValidEngine 判定引擎类型是否合法（动态注册后端时校验）。
func ValidEngine(engine string) bool { return validEngines[engine] }

// ModelRoute 为模型名到后端池的路由规则。
type ModelRoute struct {
	// Name 模型名，"*" 为兜底路由。
	Name string `yaml:"name"`
	// Backends 常规池；与 PDGroup / Splits 三选一。
	Backends []string `yaml:"backends"`
	// PDGroup 引用 PD 分离组名。
	PDGroup string `yaml:"pd_group"`
	// Splits 权重分池（金丝雀/灰度发布）：按权重把流量切分到多个子池，
	// 子池可各自指定策略；选中子池无可用后端时回退全池（可用性优先于分流比例）。
	Splits []SplitConfig `yaml:"splits"`
	// Strategy 调度策略：round_robin | random | weighted_random | least_request |
	// p2c | consistent_hash | cache_aware | expression。
	Strategy string `yaml:"strategy"`
	// Policy strategy=expression 时引用的策略名，缺省 "default"。
	Policy string `yaml:"policy"`
	// RewriteModel 转发前把请求体 model 字段改写为该值
	//（对外统一模型名 -> 后端实际部署名），空表示不改写。
	RewriteModel string `yaml:"rewrite_model"`
	// RateLimitQPS / RateLimitBurst 模型级令牌桶限流，QPS<=0 表示不限。
	RateLimitQPS   float64 `yaml:"rate_limit_qps"`
	RateLimitBurst int     `yaml:"rate_limit_burst"`
}

// SplitConfig 为一个权重子池（金丝雀分流用）。
type SplitConfig struct {
	// Name 子池名，用于指标标签与日志。
	Name     string   `yaml:"name"`
	Backends []string `yaml:"backends"`
	// Weight 相对权重（>0），流量按各子池权重占比分配。
	Weight float64 `yaml:"weight"`
	// Strategy / Policy 子池内选路策略，缺省继承所属路由。
	Strategy string `yaml:"strategy"`
	Policy   string `yaml:"policy"`
}

// PDGroupConfig 为 Prefill/Decode 分离组配置。
type PDGroupConfig struct {
	Name    string   `yaml:"name"`
	Prefill []string `yaml:"prefill"`
	Decode  []string `yaml:"decode"`
	// Links 显式声明的 NCCL/NIXL 链路；为空表示全互联（自动建链）。
	Links []NCCLLinkConfig `yaml:"nccl_links"`
	// Strategy / Policy 用于 prefill 侧选择，缺省 expression + default。
	Strategy string `yaml:"strategy"`
	Policy   string `yaml:"policy"`
}

// NCCLLinkConfig 为一条 prefill->decode 的 KV 传输链路。
type NCCLLinkConfig struct {
	Prefill string `yaml:"prefill"`
	Decode  string `yaml:"decode"`
	// BandwidthGbps 链路带宽，用于链路打分（传输并发数/带宽），缺省 100。
	BandwidthGbps float64 `yaml:"bandwidth_gbps"`
}

// AlertingConfig 为告警配置：规则表达式周期求值，状态跃迁时推送 webhook。
type AlertingConfig struct {
	Enabled bool `yaml:"enabled"`
	// Interval 规则求值周期。
	Interval Duration          `yaml:"interval"`
	Webhooks []WebhookConfig   `yaml:"webhooks"`
	Rules    []AlertRuleConfig `yaml:"rules"`
}

// WebhookConfig 为一个 webhook 通知目标。
type WebhookConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Template 消息模板：generic（原始 JSON 事件，供自动扩容器等程序消费）|
	// dingtalk | feishu | wecom | slack（各 IM 机器人格式）。
	Template string `yaml:"template"`
	// Headers 附加请求头（如认证 token）。
	Headers map[string]string `yaml:"headers"`
	Timeout Duration          `yaml:"timeout"`
	// Retries 投递失败重试次数，指数退避。
	Retries      int      `yaml:"retries"`
	RetryBackoff Duration `yaml:"retry_backoff"`
}

// AlertRuleConfig 为一条告警规则。表达式变量与调度策略表达式同一套语义：
// backend 作用域逐后端求值（running/waiting/kv_usage/raw/vars...），
// cluster 作用域逐模型路由求值（avg_waiting/max_kv_usage/available_count...）。
type AlertRuleConfig struct {
	Name string `yaml:"name"`
	// Scope 求值作用域：backend | cluster。
	Scope string `yaml:"scope"`
	// Expr 布尔表达式，连续为真达到 For 时长后进入 firing。
	Expr string   `yaml:"expr"`
	For  Duration `yaml:"for"`
	// Severity info | warning | critical。
	Severity string `yaml:"severity"`
	// RepeatInterval firing 期间的重复通知间隔，0 表示只通知一次。
	RepeatInterval Duration `yaml:"repeat_interval"`
	// Webhooks 目标名列表，空表示投递到全部 webhook。
	Webhooks []string `yaml:"webhooks"`
	// Summary 人读摘要（事件中原样携带，IM 模板作为标题）。
	Summary string            `yaml:"summary"`
	Labels  map[string]string `yaml:"labels"`
}

// AutoscaleConfig 为自动扩缩容建议配置。网关本身不执行扩缩容，
// 只产出建议事件（webhook 推送 + admin 查询 + gateway_autoscale_desired_replicas 指标），
// 由外部控制器（K8s operator / 运维机器人）落地。
type AutoscaleConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
	// Webhooks 建议事件的投递目标（引用 alerting.webhooks 中的名字）。
	Webhooks []string                `yaml:"webhooks"`
	Policies []AutoscalePolicyConfig `yaml:"policies"`
}

// AutoscalePolicyConfig 为单条模型路由的扩缩容策略。
type AutoscalePolicyConfig struct {
	// Model 模型路由名，须存在于 models。
	Model string `yaml:"model"`
	// MinReplicas / MaxReplicas 建议副本数的钳位区间，Max 为 0 表示不设上限。
	MinReplicas int `yaml:"min_replicas"`
	MaxReplicas int `yaml:"max_replicas"`
	// ScaleUpExpr / ScaleDownExpr cluster 作用域布尔表达式，为空表示该方向不启用。
	// 同时命中时扩容优先（保守偏向可用性）。
	ScaleUpExpr   string `yaml:"scale_up_expr"`
	ScaleDownExpr string `yaml:"scale_down_expr"`
	// ScaleUpStep / ScaleDownStep 单次建议的副本增减量。
	ScaleUpStep   int `yaml:"scale_up_step"`
	ScaleDownStep int `yaml:"scale_down_step"`
	// ScaleUpCooldown / ScaleDownCooldown 同方向两次建议之间的最小间隔，
	// 防止指标抖动导致建议震荡。
	ScaleUpCooldown   Duration `yaml:"scale_up_cooldown"`
	ScaleDownCooldown Duration `yaml:"scale_down_cooldown"`
}

// 各引擎类型合法值。
var validEngines = map[string]bool{
	"vllm": true, "vllm_omni": true, "sglang": true, "sglang_omni": true,
}

// 各调度策略合法值。
var validStrategies = map[string]bool{
	"round_robin": true, "random": true, "weighted_random": true,
	"least_request": true, "p2c": true, "consistent_hash": true,
	"cache_aware": true, "expression": true,
}

// webhook 消息模板合法值。
var validWebhookTemplates = map[string]bool{
	"generic": true, "dingtalk": true, "feishu": true, "wecom": true, "slack": true,
}

// DefaultPolicyName 内置策略名，未显式配置时自动注入。
const DefaultPolicyName = "default"

// 内置默认策略：过滤不健康后端，综合运行数、排队数、KV 占用与前缀命中打分。
var builtinDefaultPolicy = PolicyConfig{
	Filter: "healthy && kv_usage < 0.98",
	Score:  "running * 2.0 + waiting * 6.0 + inflight * 1.0 + kv_usage * 8.0 - prefix_match * 10.0",
}

// Load 读取并解析配置文件，随后填充默认值并校验。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults 填充所有缺省值，保证下游模块无需判空。
func (c *Config) ApplyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = 32 << 20
	}
	if c.Server.Retries < 0 {
		c.Server.Retries = 0
	} else if c.Server.Retries == 0 {
		c.Server.Retries = 2
	}
	if c.Server.UpstreamConnectTimeout <= 0 {
		c.Server.UpstreamConnectTimeout = Duration(3 * time.Second)
	}
	if c.Server.UpstreamResponseTimeout <= 0 {
		c.Server.UpstreamResponseTimeout = Duration(120 * time.Second)
	}
	if c.Server.FailureThreshold <= 0 {
		c.Server.FailureThreshold = 3
	}
	if c.Server.EjectCooldown <= 0 {
		c.Server.EjectCooldown = Duration(15 * time.Second)
	}

	if c.Metrics.ScrapeInterval <= 0 {
		c.Metrics.ScrapeInterval = Duration(2 * time.Second)
	}
	if c.Metrics.ScrapeTimeout <= 0 {
		c.Metrics.ScrapeTimeout = Duration(2 * time.Second)
	}
	if c.Metrics.HealthInterval <= 0 {
		c.Metrics.HealthInterval = Duration(5 * time.Second)
	}
	if c.Metrics.HealthTimeout <= 0 {
		c.Metrics.HealthTimeout = Duration(2 * time.Second)
	}

	if c.Prometheus.Interval <= 0 {
		c.Prometheus.Interval = Duration(10 * time.Second)
	}
	if c.Prometheus.Timeout <= 0 {
		c.Prometheus.Timeout = Duration(5 * time.Second)
	}
	for i := range c.Prometheus.Queries {
		if c.Prometheus.Queries[i].BackendLabel == "" {
			c.Prometheus.Queries[i].BackendLabel = "instance"
		}
	}

	if c.KVCache.MaxPrefixBytes <= 0 {
		c.KVCache.MaxPrefixBytes = 4096
	}
	if c.KVCache.TTL <= 0 {
		c.KVCache.TTL = Duration(10 * time.Minute)
	}
	if c.KVCache.PruneInterval <= 0 {
		c.KVCache.PruneInterval = Duration(30 * time.Second)
	}
	if c.KVCache.MatchThreshold <= 0 {
		c.KVCache.MatchThreshold = 0.3
	}
	if c.KVCache.BalanceAbsThreshold <= 0 {
		c.KVCache.BalanceAbsThreshold = 32
	}
	if c.KVCache.BalanceRelThreshold <= 0 {
		c.KVCache.BalanceRelThreshold = 1.5
	}

	if c.Session.TTL <= 0 {
		c.Session.TTL = Duration(10 * time.Minute)
	}
	if c.Session.MaxEntries <= 0 {
		c.Session.MaxEntries = 100000
	}
	if c.Queue.MaxWait <= 0 {
		c.Queue.MaxWait = Duration(5 * time.Second)
	}
	if c.Queue.MaxDepth <= 0 {
		c.Queue.MaxDepth = 1024
	}

	if c.Policies == nil {
		c.Policies = map[string]PolicyConfig{}
	}
	if _, ok := c.Policies[DefaultPolicyName]; !ok {
		c.Policies[DefaultPolicyName] = builtinDefaultPolicy
	}
	// 预设引用展开：仅在未手写表达式时填充；未知预设/冲突留给 Validate 报错。
	for name, pc := range c.Policies {
		if pc.Preset != "" && pc.Filter == "" && pc.Score == "" {
			if pr := policy.FindPreset(pc.Preset); pr != nil {
				pc.Filter, pc.Score = pr.Filter, pr.Score
				c.Policies[name] = pc
			}
		}
	}

	for i := range c.Backends {
		c.Backends[i].ApplyDefaults()
	}

	for i := range c.Models {
		m := &c.Models[i]
		if m.Strategy == "" {
			m.Strategy = "expression"
		}
		if m.Strategy == "expression" && m.Policy == "" {
			m.Policy = DefaultPolicyName
		}
		for j := range m.Splits {
			sp := &m.Splits[j]
			if sp.Weight <= 0 {
				sp.Weight = 1
			}
			if sp.Strategy == "" {
				sp.Strategy = m.Strategy
			}
			if sp.Strategy == "expression" && sp.Policy == "" {
				if m.Policy != "" {
					sp.Policy = m.Policy
				} else {
					sp.Policy = DefaultPolicyName
				}
			}
		}
		if m.RateLimitBurst <= 0 {
			m.RateLimitBurst = int(m.RateLimitQPS) * 2
			if m.RateLimitBurst <= 0 {
				m.RateLimitBurst = 1
			}
		}
	}

	for i := range c.PDGroups {
		g := &c.PDGroups[i]
		if g.Strategy == "" {
			g.Strategy = "expression"
		}
		if g.Policy == "" {
			g.Policy = DefaultPolicyName
		}
		for j := range g.Links {
			if g.Links[j].BandwidthGbps <= 0 {
				g.Links[j].BandwidthGbps = 100
			}
		}
	}

	if c.Alerting.Interval <= 0 {
		c.Alerting.Interval = Duration(5 * time.Second)
	}
	for i := range c.Alerting.Webhooks {
		w := &c.Alerting.Webhooks[i]
		if w.Template == "" {
			w.Template = "generic"
		}
		if w.Timeout <= 0 {
			w.Timeout = Duration(5 * time.Second)
		}
		if w.Retries < 0 {
			w.Retries = 0
		} else if w.Retries == 0 {
			w.Retries = 3
		}
		if w.RetryBackoff <= 0 {
			w.RetryBackoff = Duration(time.Second)
		}
	}
	for i := range c.Alerting.Rules {
		r := &c.Alerting.Rules[i]
		if r.Scope == "" {
			r.Scope = "backend"
		}
		if r.Severity == "" {
			r.Severity = "warning"
		}
		if r.Labels == nil {
			r.Labels = map[string]string{}
		}
	}

	if c.Cluster.KeyPrefix == "" {
		c.Cluster.KeyPrefix = "gateway"
	}
	if c.Cluster.HeartbeatInterval <= 0 {
		c.Cluster.HeartbeatInterval = Duration(3 * time.Second)
	}
	if c.Cluster.HeartbeatTTL <= 0 {
		c.Cluster.HeartbeatTTL = Duration(10 * time.Second)
	}
	if c.Cluster.LeaderTTL <= 0 {
		c.Cluster.LeaderTTL = Duration(15 * time.Second)
	}
	if c.Cluster.RateLimitMode == "" {
		c.Cluster.RateLimitMode = "distributed"
	}
	if c.Cluster.SessionMode == "" {
		c.Cluster.SessionMode = "shared"
	}
	if c.Cluster.OpTimeout <= 0 {
		c.Cluster.OpTimeout = Duration(500 * time.Millisecond)
	}

	if c.Store.MaxOpenConns <= 0 {
		c.Store.MaxOpenConns = 4
	}
	if c.Store.OpTimeout <= 0 {
		c.Store.OpTimeout = Duration(3 * time.Second)
	}
	if c.Store.ReconcileInterval <= 0 {
		c.Store.ReconcileInterval = Duration(30 * time.Second)
	}

	if c.Autoscale.Interval <= 0 {
		c.Autoscale.Interval = Duration(15 * time.Second)
	}
	for i := range c.Autoscale.Policies {
		p := &c.Autoscale.Policies[i]
		if p.MinReplicas <= 0 {
			p.MinReplicas = 1
		}
		if p.ScaleUpStep <= 0 {
			p.ScaleUpStep = 1
		}
		if p.ScaleDownStep <= 0 {
			p.ScaleDownStep = 1
		}
		if p.ScaleUpCooldown <= 0 {
			p.ScaleUpCooldown = Duration(time.Minute)
		}
		if p.ScaleDownCooldown <= 0 {
			p.ScaleDownCooldown = Duration(5 * time.Minute)
		}
	}

	for i := range c.Rollouts {
		r := &c.Rollouts[i]
		if r.Interval <= 0 {
			r.Interval = Duration(2 * time.Minute)
		}
		if r.AutoStart == nil {
			v := true
			r.AutoStart = &v
		}
	}

	if c.Tracing.Insecure == nil {
		v := true
		c.Tracing.Insecure = &v
	}
	if c.Tracing.SampleRatio == nil {
		v := 1.0
		c.Tracing.SampleRatio = &v
	}
	if c.Tracing.ServiceName == "" {
		c.Tracing.ServiceName = "ai-gateway"
	}
}

// Validate 做全量静态校验，任何引用错误都在启动期暴露。
func (c *Config) Validate() error {
	for name, pc := range c.Policies {
		if pc.Preset == "" {
			continue
		}
		pr := policy.FindPreset(pc.Preset)
		if pr == nil {
			return fmt.Errorf("配置校验失败: 策略 %s 引用了不存在的预设 %q（可选: %s）",
				name, pc.Preset, strings.Join(policy.PresetNames(), " / "))
		}
		// ApplyDefaults 只在未手写表达式时填充；此处不相等即为两者同时给出。
		if pc.Filter != pr.Filter || pc.Score != pr.Score {
			return fmt.Errorf("配置校验失败: 策略 %s 的 preset 与手写 filter/score 只能二选一", name)
		}
	}

	if len(c.Backends) == 0 {
		return fmt.Errorf("配置校验失败: 至少需要一个后端")
	}
	ids := map[string]bool{}
	for _, b := range c.Backends {
		if b.ID == "" {
			return fmt.Errorf("配置校验失败: 后端缺少 id")
		}
		if ids[b.ID] {
			return fmt.Errorf("配置校验失败: 后端 id 重复: %s", b.ID)
		}
		ids[b.ID] = true
		if b.URL == "" {
			return fmt.Errorf("配置校验失败: 后端 %s 缺少 url", b.ID)
		}
		if !validEngines[b.Engine] {
			return fmt.Errorf("配置校验失败: 后端 %s 的 engine %q 非法，可选 vllm/vllm_omni/sglang/sglang_omni", b.ID, b.Engine)
		}
	}

	groups := map[string]bool{}
	for _, g := range c.PDGroups {
		if g.Name == "" {
			return fmt.Errorf("配置校验失败: pd_group 缺少 name")
		}
		if groups[g.Name] {
			return fmt.Errorf("配置校验失败: pd_group 名称重复: %s", g.Name)
		}
		groups[g.Name] = true
		if len(g.Prefill) == 0 || len(g.Decode) == 0 {
			return fmt.Errorf("配置校验失败: pd_group %s 的 prefill/decode 池不能为空", g.Name)
		}
		members := map[string]bool{}
		for _, id := range append(append([]string{}, g.Prefill...), g.Decode...) {
			if !ids[id] {
				return fmt.Errorf("配置校验失败: pd_group %s 引用了不存在的后端 %s", g.Name, id)
			}
			members[id] = true
		}
		for _, l := range g.Links {
			if !members[l.Prefill] || !members[l.Decode] {
				return fmt.Errorf("配置校验失败: pd_group %s 的链路 %s->%s 引用了组外后端", g.Name, l.Prefill, l.Decode)
			}
		}
		if !validStrategies[g.Strategy] {
			return fmt.Errorf("配置校验失败: pd_group %s 的 strategy %q 非法", g.Name, g.Strategy)
		}
		if _, ok := c.Policies[g.Policy]; g.Strategy == "expression" && !ok {
			return fmt.Errorf("配置校验失败: pd_group %s 引用了不存在的策略 %s", g.Name, g.Policy)
		}
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("配置校验失败: 至少需要一条模型路由")
	}
	names := map[string]bool{}
	for _, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("配置校验失败: 模型路由缺少 name")
		}
		if names[m.Name] {
			return fmt.Errorf("配置校验失败: 模型路由重复: %s", m.Name)
		}
		names[m.Name] = true
		if !validStrategies[m.Strategy] {
			return fmt.Errorf("配置校验失败: 模型 %s 的 strategy %q 非法", m.Name, m.Strategy)
		}
		switch {
		case m.PDGroup != "":
			if len(m.Backends) > 0 || len(m.Splits) > 0 {
				return fmt.Errorf("配置校验失败: 模型 %s 的 backends/splits 与 pd_group 只能选其一", m.Name)
			}
			if !groups[m.PDGroup] {
				return fmt.Errorf("配置校验失败: 模型 %s 引用了不存在的 pd_group %s", m.Name, m.PDGroup)
			}
		case len(m.Splits) > 0:
			if len(m.Backends) > 0 {
				return fmt.Errorf("配置校验失败: 模型 %s 的 backends 与 splits 只能二选一", m.Name)
			}
			splitNames := map[string]bool{}
			for _, sp := range m.Splits {
				if sp.Name == "" {
					return fmt.Errorf("配置校验失败: 模型 %s 存在缺少 name 的 split", m.Name)
				}
				if splitNames[sp.Name] {
					return fmt.Errorf("配置校验失败: 模型 %s 的 split 名称重复: %s", m.Name, sp.Name)
				}
				splitNames[sp.Name] = true
				if len(sp.Backends) == 0 {
					return fmt.Errorf("配置校验失败: 模型 %s 的 split %s 未配置 backends", m.Name, sp.Name)
				}
				for _, id := range sp.Backends {
					if !ids[id] {
						return fmt.Errorf("配置校验失败: 模型 %s 的 split %s 引用了不存在的后端 %s", m.Name, sp.Name, id)
					}
				}
				if !validStrategies[sp.Strategy] {
					return fmt.Errorf("配置校验失败: 模型 %s 的 split %s 的 strategy %q 非法", m.Name, sp.Name, sp.Strategy)
				}
				if sp.Strategy == "expression" {
					if _, ok := c.Policies[sp.Policy]; !ok {
						return fmt.Errorf("配置校验失败: 模型 %s 的 split %s 引用了不存在的策略 %s", m.Name, sp.Name, sp.Policy)
					}
				}
			}
		default:
			if len(m.Backends) == 0 {
				return fmt.Errorf("配置校验失败: 模型 %s 未配置 backends、splits 或 pd_group", m.Name)
			}
			for _, id := range m.Backends {
				if !ids[id] {
					return fmt.Errorf("配置校验失败: 模型 %s 引用了不存在的后端 %s", m.Name, id)
				}
			}
		}
		if m.Strategy == "expression" {
			if _, ok := c.Policies[m.Policy]; !ok {
				return fmt.Errorf("配置校验失败: 模型 %s 引用了不存在的策略 %s", m.Name, m.Policy)
			}
		}
	}

	webhooks := map[string]bool{}
	for _, w := range c.Alerting.Webhooks {
		if w.Name == "" {
			return fmt.Errorf("配置校验失败: webhook 缺少 name")
		}
		if webhooks[w.Name] {
			return fmt.Errorf("配置校验失败: webhook 名称重复: %s", w.Name)
		}
		webhooks[w.Name] = true
		if w.URL == "" {
			return fmt.Errorf("配置校验失败: webhook %s 缺少 url", w.Name)
		}
		if !validWebhookTemplates[w.Template] {
			return fmt.Errorf("配置校验失败: webhook %s 的 template %q 非法，可选 generic/dingtalk/feishu/wecom/slack", w.Name, w.Template)
		}
	}
	ruleNames := map[string]bool{}
	for _, r := range c.Alerting.Rules {
		if r.Name == "" {
			return fmt.Errorf("配置校验失败: 告警规则缺少 name")
		}
		if ruleNames[r.Name] {
			return fmt.Errorf("配置校验失败: 告警规则名称重复: %s", r.Name)
		}
		ruleNames[r.Name] = true
		if r.Expr == "" {
			return fmt.Errorf("配置校验失败: 告警规则 %s 缺少 expr", r.Name)
		}
		if r.Scope != "backend" && r.Scope != "cluster" {
			return fmt.Errorf("配置校验失败: 告警规则 %s 的 scope %q 非法，可选 backend/cluster", r.Name, r.Scope)
		}
		if r.Severity != "info" && r.Severity != "warning" && r.Severity != "critical" {
			return fmt.Errorf("配置校验失败: 告警规则 %s 的 severity %q 非法，可选 info/warning/critical", r.Name, r.Severity)
		}
		for _, name := range r.Webhooks {
			if !webhooks[name] {
				return fmt.Errorf("配置校验失败: 告警规则 %s 引用了不存在的 webhook %s", r.Name, name)
			}
		}
	}

	for _, name := range c.Autoscale.Webhooks {
		if !webhooks[name] {
			return fmt.Errorf("配置校验失败: autoscale 引用了不存在的 webhook %s", name)
		}
	}
	scaleModels := map[string]bool{}
	for _, p := range c.Autoscale.Policies {
		if p.Model == "" {
			return fmt.Errorf("配置校验失败: autoscale 策略缺少 model")
		}
		if scaleModels[p.Model] {
			return fmt.Errorf("配置校验失败: autoscale 策略的 model 重复: %s", p.Model)
		}
		scaleModels[p.Model] = true
		if !names[p.Model] {
			return fmt.Errorf("配置校验失败: autoscale 策略引用了不存在的模型路由 %s", p.Model)
		}
		if p.ScaleUpExpr == "" && p.ScaleDownExpr == "" {
			return fmt.Errorf("配置校验失败: autoscale 策略 %s 至少需要 scale_up_expr 或 scale_down_expr 之一", p.Model)
		}
		if p.MaxReplicas > 0 && p.MaxReplicas < p.MinReplicas {
			return fmt.Errorf("配置校验失败: autoscale 策略 %s 的 max_replicas 小于 min_replicas", p.Model)
		}
	}

	rolloutModels := map[string]bool{}
	for _, r := range c.Rollouts {
		if r.Model == "" || r.Canary == "" {
			return fmt.Errorf("配置校验失败: rollout 缺少 model 或 canary")
		}
		if rolloutModels[r.Model] {
			return fmt.Errorf("配置校验失败: rollout 的 model 重复: %s", r.Model)
		}
		rolloutModels[r.Model] = true
		var route *ModelRoute
		for i := range c.Models {
			if c.Models[i].Name == r.Model {
				route = &c.Models[i]
				break
			}
		}
		if route == nil {
			return fmt.Errorf("配置校验失败: rollout 引用了不存在的模型路由 %s", r.Model)
		}
		if len(route.Splits) < 2 {
			return fmt.Errorf("配置校验失败: rollout 模型 %s 需要至少两个 splits 子池（金丝雀 + 稳定池）", r.Model)
		}
		hasCanary := false
		for _, sp := range route.Splits {
			if sp.Name == r.Canary {
				hasCanary = true
			}
		}
		if !hasCanary {
			return fmt.Errorf("配置校验失败: rollout 模型 %s 不存在名为 %s 的子池", r.Model, r.Canary)
		}
		if len(r.Steps) == 0 {
			return fmt.Errorf("配置校验失败: rollout 模型 %s 缺少 steps 阶梯", r.Model)
		}
		prev := 0.0
		for _, s := range r.Steps {
			if s <= prev || s > 100 {
				return fmt.Errorf("配置校验失败: rollout 模型 %s 的 steps 须在 (0,100] 严格递增", r.Model)
			}
			prev = s
		}
		if r.PromoteExpr == "" || r.RollbackExpr == "" {
			return fmt.Errorf("配置校验失败: rollout 模型 %s 的 promote_expr 与 rollback_expr 均为必填", r.Model)
		}
		for _, name := range r.Webhooks {
			if !webhooks[name] {
				return fmt.Errorf("配置校验失败: rollout 模型 %s 引用了不存在的 webhook %s", r.Model, name)
			}
		}
	}

	if c.Tracing.Enabled {
		if c.Tracing.Endpoint == "" {
			return fmt.Errorf("配置校验失败: tracing 已启用但缺少 endpoint")
		}
		if r := *c.Tracing.SampleRatio; r < 0 || r > 1 {
			return fmt.Errorf("配置校验失败: tracing.sample_ratio 须在 0~1 之间: %g", r)
		}
	}

	if c.Store.Enabled {
		if c.Store.Driver != "postgres" && c.Store.Driver != "mysql" {
			return fmt.Errorf("配置校验失败: store.driver %q 非法，可选 postgres/mysql", c.Store.Driver)
		}
		if c.Store.DSN == "" {
			return fmt.Errorf("配置校验失败: store.enabled 时必须配置 dsn")
		}
	}

	if c.Cluster.Enabled {
		if c.Cluster.RedisAddr == "" && len(c.Cluster.RedisAddrs) == 0 {
			return fmt.Errorf("配置校验失败: cluster.enabled 时必须配置 redis_addr 或 redis_addrs")
		}
		if c.Cluster.RedisMasterName != "" && len(c.Cluster.RedisAddrs) == 0 {
			return fmt.Errorf("配置校验失败: 配置了 redis_master_name（Sentinel 模式）时必须用 redis_addrs 提供 Sentinel 地址列表")
		}
		if c.Cluster.RateLimitMode != "distributed" && c.Cluster.RateLimitMode != "local" {
			return fmt.Errorf("配置校验失败: cluster.rate_limit_mode %q 非法，可选 distributed/local", c.Cluster.RateLimitMode)
		}
		if c.Cluster.SessionMode != "shared" && c.Cluster.SessionMode != "local" {
			return fmt.Errorf("配置校验失败: cluster.session_mode %q 非法，可选 shared/local", c.Cluster.SessionMode)
		}
	}
	return nil
}
