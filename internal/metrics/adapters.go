// 指标名适配层：把不同引擎家族的原生 Prometheus 指标名映射到统一的快照字段。
// 每个字段给出候选名列表（新老版本指标名并存），按顺序取第一个命中的。
package metrics

// adapter 一个引擎家族的指标名映射表。
type adapter struct {
	// running / waiting / kvUsage / hitRate 为 gauge 语义候选名。
	running []string
	waiting []string
	kvUsage []string
	hitRate []string
	// genThroughputGauge 直接暴露吞吐 gauge 的候选名（sglang:gen_throughput）。
	genThroughputGauge []string
	// genTokensCounter 生成 token 累计 counter 的候选名，取速率派生吞吐。
	genTokensCounter []string
	// hitQueriesCounter / hitHitsCounter 命中率需由 counter 对推导时使用
	//（新版 vllm 移除了 hit_rate gauge，只保留 queries/hits 累计值）。
	hitQueriesCounter []string
	hitHitsCounter    []string
}

// adapters 按引擎家族索引；omni 变体通过 EngineType.Family() 复用基础引擎表。
var adapters = map[string]adapter{
	"vllm": {
		running: []string{"vllm:num_requests_running"},
		waiting: []string{"vllm:num_requests_waiting"},
		kvUsage: []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"},
		hitRate: []string{"vllm:gpu_prefix_cache_hit_rate"},
		hitQueriesCounter: []string{
			"vllm:gpu_prefix_cache_queries_total", "vllm:prefix_cache_queries_total",
		},
		hitHitsCounter: []string{
			"vllm:gpu_prefix_cache_hits_total", "vllm:prefix_cache_hits_total",
		},
		genTokensCounter: []string{"vllm:generation_tokens_total"},
	},
	"sglang": {
		running:            []string{"sglang:num_running_reqs"},
		waiting:            []string{"sglang:num_queue_reqs"},
		kvUsage:            []string{"sglang:token_usage", "sglang:kv_cache_usage"},
		hitRate:            []string{"sglang:cache_hit_rate"},
		genThroughputGauge: []string{"sglang:gen_throughput"},
		genTokensCounter:   []string{"sglang:generation_tokens_total"},
	},
}

// pick 依次尝试候选名，返回第一个存在的值。
func pick(raw map[string]float64, candidates []string) (float64, bool) {
	for _, name := range candidates {
		if v, ok := raw[name]; ok {
			return v, true
		}
	}
	return 0, false
}
