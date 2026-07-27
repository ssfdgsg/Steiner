// Package mockbackend 模拟 vLLM / vLLM-Omni / SGLang / SGLang-Omni 的对外行为，
// 用于集成测试与本地冒烟，不依赖 GPU。
//
// 能力：
//   - /v1/chat/completions、/v1/completions：JSON 或 SSE 流式应答；
//   - /metrics：按引擎家族渲染指标文本（vllm: / sglang: 前缀）；
//   - /health：可编程返回 200/503；
//   - /mock/state：设置指标值与健康状态（故障注入）；
//   - /mock/fault：注入拒答（5xx）与流中断；
//   - PD 模拟：vllm 家族请求携带 kv_transfer_params(do_remote_decode) 时按
//     prefill 语义返回 KV 句柄；记录最近一次请求体供用例断言
//     bootstrap/kv_transfer_params 注入。
package mockbackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// State 可编程的引擎状态。
type State struct {
	Healthy      bool    `json:"healthy"`
	Running      float64 `json:"running"`
	Waiting      float64 `json:"waiting"`
	KVUsage      float64 `json:"kv_usage"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	GenTokens    float64 `json:"gen_tokens"` // 累计生成 token 数（counter）
}

// Fault 故障注入配置。
type Fault struct {
	// FailCode >0 时补全接口直接返回该状态码。
	FailCode int `json:"fail_code"`
	// CutStream true 时 SSE 流在第一块后中断。
	CutStream bool `json:"cut_stream"`
	// TTFTDelay 首字节前延迟。
	TTFTDelayMS int `json:"ttft_delay_ms"`
}

// Mock 单个模拟后端。
type Mock struct {
	Engine string // vllm | vllm_omni | sglang | sglang_omni

	mu         sync.Mutex
	state      State
	fault      Fault
	lastBody   map[string]interface{} // 最近一次补全请求体（断言用）
	lastHeader http.Header            // 最近一次补全请求头（断言 traceparent 等传播头用）
	reqCount   int
}

// New 构造模拟后端，初始健康、零负载。
func New(engine string) *Mock {
	return &Mock{Engine: engine, state: State{Healthy: true}}
}

// family 引擎家族（决定指标前缀与 PD 协议）。
func (m *Mock) family() string {
	if m.Engine == "sglang" || m.Engine == "sglang_omni" {
		return "sglang"
	}
	return "vllm"
}

// LastBody 返回最近一次补全请求体的拷贝。
func (m *Mock) LastBody() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]interface{}{}
	for k, v := range m.lastBody {
		out[k] = v
	}
	return out
}

// LastHeader 返回最近一次补全请求的请求头拷贝。
func (m *Mock) LastHeader() http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastHeader == nil {
		return http.Header{}
	}
	return m.lastHeader.Clone()
}

// ReqCount 已处理的补全请求数。
func (m *Mock) ReqCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reqCount
}

// SetState 直接设置状态（测试便捷入口，等价 POST /mock/state）。
func (m *Mock) SetState(s State) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

// SetFault 直接设置故障（等价 POST /mock/fault）。
func (m *Mock) SetFault(f Fault) {
	m.mu.Lock()
	m.fault = f
	m.mu.Unlock()
}

// Handler 返回完整路由。
func (m *Mock) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", m.handleHealth)
	mux.HandleFunc("GET /metrics", m.handleMetrics)
	mux.HandleFunc("POST /v1/chat/completions", m.handleCompletion)
	mux.HandleFunc("POST /v1/completions", m.handleCompletion)
	mux.HandleFunc("POST /mock/state", m.handleSetState)
	mux.HandleFunc("POST /mock/fault", m.handleSetFault)
	return mux
}

func (m *Mock) handleHealth(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	healthy := m.state.Healthy
	m.mu.Unlock()
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (m *Mock) handleSetState(w http.ResponseWriter, r *http.Request) {
	var s State
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.SetState(s)
	w.WriteHeader(http.StatusOK)
}

func (m *Mock) handleSetFault(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.SetFault(f)
	w.WriteHeader(http.StatusOK)
}

// handleMetrics 按引擎家族渲染 Prometheus 文本。
func (m *Mock) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	s := m.state
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if m.family() == "vllm" {
		fmt.Fprintf(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"m\"} %g\n", s.Running)
		fmt.Fprintf(w, "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} %g\n", s.Waiting)
		fmt.Fprintf(w, "# TYPE vllm:gpu_cache_usage_perc gauge\nvllm:gpu_cache_usage_perc{model_name=\"m\"} %g\n", s.KVUsage)
		fmt.Fprintf(w, "# TYPE vllm:gpu_prefix_cache_hit_rate gauge\nvllm:gpu_prefix_cache_hit_rate{model_name=\"m\"} %g\n", s.CacheHitRate)
		fmt.Fprintf(w, "# TYPE vllm:generation_tokens_total counter\nvllm:generation_tokens_total{model_name=\"m\"} %g\n", s.GenTokens)
		return
	}
	fmt.Fprintf(w, "# TYPE sglang:num_running_reqs gauge\nsglang:num_running_reqs{model_name=\"m\"} %g\n", s.Running)
	fmt.Fprintf(w, "# TYPE sglang:num_queue_reqs gauge\nsglang:num_queue_reqs{model_name=\"m\"} %g\n", s.Waiting)
	fmt.Fprintf(w, "# TYPE sglang:token_usage gauge\nsglang:token_usage{model_name=\"m\"} %g\n", s.KVUsage)
	fmt.Fprintf(w, "# TYPE sglang:cache_hit_rate gauge\nsglang:cache_hit_rate{model_name=\"m\"} %g\n", s.CacheHitRate)
	fmt.Fprintf(w, "# TYPE sglang:gen_throughput gauge\nsglang:gen_throughput{model_name=\"m\"} %g\n", s.GenTokens)
}

// handleCompletion 补全接口：记录请求体，按故障配置与请求参数应答。
func (m *Mock) handleCompletion(w http.ResponseWriter, r *http.Request) {
	var doc map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.lastBody = doc
	m.lastHeader = r.Header.Clone()
	m.reqCount++
	fault := m.fault
	m.mu.Unlock()

	if fault.FailCode > 0 {
		http.Error(w, "注入故障", fault.FailCode)
		return
	}
	if fault.TTFTDelayMS > 0 {
		time.Sleep(time.Duration(fault.TTFTDelayMS) * time.Millisecond)
	}

	stream, _ := doc["stream"].(bool)
	if stream {
		m.streamCompletion(w, fault)
		return
	}

	// vllm PD prefill 语义：请求携带 do_remote_decode 时返回 KV 传输句柄。
	resp := map[string]interface{}{
		"id":      "mock-cmpl-1",
		"object":  "chat.completion",
		"model":   doc["model"],
		"choices": []interface{}{map[string]interface{}{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": "模拟应答"}}},
		// 与真实引擎一致携带 usage，供网关 token 用量指标链路验证。
		"usage": map[string]interface{}{"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
	}
	if ktp, ok := doc["kv_transfer_params"].(map[string]interface{}); ok {
		if remote, _ := ktp["do_remote_decode"].(bool); remote {
			resp["kv_transfer_params"] = map[string]interface{}{
				"do_remote_prefill": true,
				"remote_engine_id":  "mock-engine",
				"remote_block_ids":  []interface{}{1, 2, 3},
				"remote_host":       "127.0.0.1",
				"remote_port":       14579,
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// streamCompletion 输出 SSE 流（三块 + [DONE]），可注入中断。
func (m *Mock) streamCompletion(w http.ResponseWriter, fault Fault) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	chunks := []string{"模拟", "流式", "应答"}
	for i, c := range chunks {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
		if fl != nil {
			fl.Flush()
		}
		if fault.CutStream && i == 0 {
			// 模拟流中断：直接返回，不发 [DONE]。
			return
		}
	}
	// 与 vLLM/SGLang 一致：末块携带 usage（中间块为 null）。
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
