// 内置调度方案预设库：把推理调度的常见优化目标沉淀为可一键切换的
// filter/score 表达式组合。前端经 GET /admin/presets 拉取清单、
// POST /admin/presets/{name}/apply 切换；配置文件 policies.<名>.preset
// 也可直接引用。
//
// 预设只使用 Go 侧预计算的保证存在的变量（running/waiting/kv_usage/
// prefix_match/ttft_ewma/preempt_rate 等），不依赖 raw/vars 的动态键，
// 保证任何引擎组合下都可安全求值。
package policy

// Preset 一个内置调度方案。
type Preset struct {
	// Name 方案标识（API 与配置引用键）。
	Name string `json:"name"`
	// Title 简短中文名（前端展示）。
	Title string `json:"title"`
	// Description 适用场景与取舍说明（前端展示）。
	Description string `json:"description"`
	Filter      string `json:"filter"`
	Score       string `json:"score"`
}

// Presets 全部内置方案（有序，前端按序展示）。
var Presets = []Preset{
	{
		Name:  "balanced",
		Title: "均衡（默认）",
		Description: "通用场景的均衡方案：综合运行数、排队数、KV 占用与前缀命中打分，" +
			"无明显偏好，适合作为基线。",
		Filter: "healthy && kv_usage < 0.98",
		Score:  "running * 2.0 + waiting * 6.0 + inflight * 1.0 + kv_usage * 8.0 - prefix_match * 10.0",
	},
	{
		Name:  "cache_affinity",
		Title: "缓存亲和优先",
		Description: "多轮对话、固定 system prompt 的 RAG 场景：大幅提高前缀命中权重，" +
			"尽量把同前缀请求路由回持有 KV cache 的后端，显著降低 prefill 开销与 TTFT；" +
			"负载项仍参与打分以避免热点。",
		Filter: "healthy && kv_usage < 0.95",
		Score:  "running * 2.0 + waiting * 5.0 + kv_usage * 6.0 + ttft_ewma * 5.0 - prefix_match * 25.0",
	},
	{
		Name:  "latency_first",
		Title: "低时延优先",
		Description: "交互式应用（对话、补全）：以网关实测 TTFT 指数滑动均值做反馈闭环，" +
			"避开首字节慢与排队深的后端；过滤条件收紧（KV 水位 0.90、排队上限 32）" +
			"以牺牲部分容量换取时延稳定。",
		Filter: "healthy && kv_usage < 0.90 && waiting < 32",
		Score: "ttft_ewma * 40.0 + waiting * 8.0 + running * 2.0 + inflight * 1.5" +
			" + preempt_rate * 20.0 - prefix_match * 8.0",
	},
	{
		Name:  "preemption_safe",
		Title: "显存保守（抢占规避）",
		Description: "长上下文、高压场景：KV 水位收紧到 0.85（PagedAttention 下接近满载" +
			"会触发抢占/换出，代价极高），叠加抢占速率强惩罚与 KV 占用二次方惩罚，" +
			"宁可牺牲吞吐也要避免请求被抢占重算。",
		Filter: "healthy && kv_usage < 0.85",
		Score: "preempt_rate * 100.0 + kv_usage * kv_usage * 30.0 + waiting * 6.0" +
			" + running * 2.0 - prefix_match * 10.0",
	},
	{
		Name:  "throughput_first",
		Title: "吞吐优先",
		Description: "离线批量/异步任务：弱化运行数权重（连续批处理下批内并行几乎免费，" +
			"应尽量填满批槽位），重点规避排队堆积与 KV 高水位，" +
			"并向实测生成吞吐更高的后端倾斜。",
		Filter: "healthy && kv_usage < 0.92",
		Score: "waiting * 10.0 + kv_usage * 10.0 + running * 0.5 + inflight * 0.5" +
			" - gen_tps * 0.02 - prefix_match * 6.0",
	},
}

// PresetNames 全部预设名（校验错误提示用）。
func PresetNames() []string {
	out := make([]string, len(Presets))
	for i := range Presets {
		out[i] = Presets[i].Name
	}
	return out
}

// FindPreset 按名查找，不存在返回 nil。
func FindPreset(name string) *Preset {
	for i := range Presets {
		if Presets[i].Name == name {
			return &Presets[i]
		}
	}
	return nil
}

// MatchPreset 返回策略槽位当前生效方案的预设名；未使用预设返回 "custom"。
// 用于展示"当前生效方案"。
//
// L3 修复：旧实现以"表达式字符串与预设完全相等"反查预设名，导致手写、
// 恰好与预设字符串逐字相同的 filter/score（典型：内置默认策略与 balanced
// 表达式完全相同）被管理面/状态视图误标为"使用了预设"。修复后预设名
// 只来自**显式声明**：explicitPreset 为配置中显式声明的 preset 字段
// （config.PolicyConfig.Preset），非空且指向存在的预设才返回该预设名；
// 手写表达式（即使与预设字符串逐字相同）一律视为自定义，返回 "custom"。
//
// 变参保持旧两参调用点（internal/server 的 handlePresetsGet）编译兼容：
// 调用方必须把配置声明的预设字段传进来才能正确展示来源，不传即视为
// 手写表达式，返回 "custom"。
func MatchPreset(filter, score string, explicitPreset ...string) string {
	if len(explicitPreset) > 0 && explicitPreset[0] != "" && FindPreset(explicitPreset[0]) != nil {
		return explicitPreset[0]
	}
	return "custom"
}
