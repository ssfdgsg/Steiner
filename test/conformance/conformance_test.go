// 真实引擎 conformance 测试：对着真实 vLLM（CPU 小模型）验证网关的协议边界。
// 由 scripts/conformance.sh 编排运行（docker 起引擎 + 拉起网关后执行）；
// 未设置 GATEWAY_CONFORMANCE=1 时整包跳过，不影响常规 make test。
//
// 网关视角的验收点：
//  1. 指标名适配：真实引擎 /metrics 中存在 adapters 表的候选指标名；
//  2. 采集与健康：网关侧 scrape_up=1、healthy=1；
//  3. 模型名改写：对外名 gw-conf-model 改写为部署名（改写失效引擎必 404）；
//  4. 非流式补全：内容非空、X-Upstream-Backend 回写；
//  5. SSE 流式：逐块 data: 帧、[DONE] 收尾、TTFT 直方图有样本；
//  6. usage 记账：completion token 计数增长（流式经 include_usage 尾块）。
package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func base(t *testing.T) string {
	t.Helper()
	if os.Getenv("GATEWAY_CONFORMANCE") != "1" {
		t.Skip("未设置 GATEWAY_CONFORMANCE=1，跳过真实引擎验证（由 make conformance 编排运行）")
	}
	if v := os.Getenv("CONFORMANCE_GATEWAY"); v != "" {
		return v
	}
	return "http://127.0.0.1:18190"
}

func vllmBase() string {
	if v := os.Getenv("CONFORMANCE_VLLM"); v != "" {
		return v
	}
	return "http://127.0.0.1:18100"
}

var client = &http.Client{Timeout: 120 * time.Second}

// fetch GET 并读全响应体。
func fetch(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// TestMetricNameAdapters 直接抓真实引擎的 /metrics，断言 adapters 表
// （internal/metrics/adapters.go 的 vllm 家族）候选指标名真实存在。
// 失败信息附上引擎实际暴露的 vllm: 指标名清单，便于直接修表。
func TestMetricNameAdapters(t *testing.T) {
	base(t)
	code, body := fetch(t, vllmBase()+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("引擎 /metrics 返回 %d", code)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i > 0 {
			name = line[:i]
		}
		names[name] = true
	}

	// 与 internal/metrics/adapters.go 的 vllm 家族候选表保持一致。
	checks := map[string][]string{
		"running":  {"vllm:num_requests_running"},
		"waiting":  {"vllm:num_requests_waiting"},
		"kv_usage": {"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"},
		"hit_rate": {
			"vllm:gpu_prefix_cache_hit_rate",
			"vllm:gpu_prefix_cache_queries_total", "vllm:prefix_cache_queries_total",
		},
	}
	var actual []string
	for n := range names {
		if strings.HasPrefix(n, "vllm:") {
			actual = append(actual, n)
		}
	}
	for field, candidates := range checks {
		hit := false
		for _, c := range candidates {
			if names[c] {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("适配字段 %s 的候选名 %v 均未命中真实引擎；引擎实际暴露的 vllm: 指标:\n%s",
				field, candidates, strings.Join(actual, "\n"))
		}
	}
}

// TestHealthyAndScrape 网关视角：健康检查通过、指标直采成功。
func TestHealthyAndScrape(t *testing.T) {
	gw := base(t)
	waitFor(t, 30*time.Second, "后端 healthy=1 且 scrape_up=1", func() bool {
		_, body := fetch(t, gw+"/metrics")
		s := string(body)
		return strings.Contains(s, `gateway_backend_healthy{backend="vllm-real"} 1`) &&
			strings.Contains(s, `gateway_backend_scrape_up{backend="vllm-real"} 1`)
	})
}

// TestModelsAndNonStream /v1/models 清单 + 非流式补全 + 模型名改写。
func TestModelsAndNonStream(t *testing.T) {
	gw := base(t)

	code, body := fetch(t, gw+"/v1/models")
	if code != http.StatusOK || !bytes.Contains(body, []byte("gw-conf-model")) {
		t.Fatalf("/v1/models 异常: %d %s", code, body)
	}

	payload := `{"model":"gw-conf-model","max_tokens":16,"messages":[{"role":"user","content":"用一句话介绍你自己"}]}`
	resp, err := client.Post(gw+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("补全请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// 模型名改写失效时真实引擎在此返回 404（unknown model）。
		t.Fatalf("非流式补全返回 %d: %s", resp.StatusCode, raw)
	}
	if resp.Header.Get("X-Upstream-Backend") != "vllm-real" {
		t.Fatalf("X-Upstream-Backend 缺失或不符: %q", resp.Header.Get("X-Upstream-Backend"))
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		t.Fatalf("响应体解析失败: %v %s", err, raw)
	}
	if out.Choices[0].Message.Content == "" {
		t.Fatalf("补全内容为空: %s", raw)
	}
}

// TestStreamSSE 流式补全：data: 帧、[DONE] 收尾、include_usage 尾块；
// 随后核对网关 TTFT 直方图与 completion token 计数。
func TestStreamSSE(t *testing.T) {
	gw := base(t)
	payload := `{"model":"gw-conf-model","max_tokens":24,"stream":true,` +
		`"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"user","content":"数到五"}]}`
	resp, err := client.Post(gw+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("流式补全返回 %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 不是 SSE: %q", ct)
	}

	var dataFrames int
	var sawDone, sawUsage bool
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			break
		}
		dataFrames++
		if strings.Contains(data, `"completion_tokens"`) {
			sawUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取 SSE 流失败: %v", err)
	}
	if dataFrames < 2 {
		t.Fatalf("SSE 分帧异常，仅收到 %d 个 data 帧", dataFrames)
	}
	if !sawDone {
		t.Fatal("SSE 未以 [DONE] 收尾")
	}
	if !sawUsage {
		t.Fatal("include_usage 尾块未透传（usage 记账依赖它）")
	}

	// 指标核对：TTFT 有样本、completion token 计数为正。
	waitFor(t, 10*time.Second, "TTFT 样本与 token 计数", func() bool {
		_, body := fetch(t, gw+"/metrics")
		s := string(body)
		return strings.Contains(s, `gateway_time_to_first_byte_seconds_count{backend="vllm-real"`) &&
			metricPositive(s, "gateway_completion_tokens_total")
	})
}

// metricPositive 判定指标文本中某计数器存在且值大于 0。
func metricPositive(text, name string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, name) {
			var v float64
			if i := strings.LastIndex(line, " "); i > 0 {
				if _, err := fmt.Sscanf(line[i+1:], "%g", &v); err == nil && v > 0 {
					return true
				}
			}
		}
	}
	return false
}

// TestUnknownModel 未配置的模型且无兜底路由应 404（错误路径的协议闭环）。
func TestUnknownModel(t *testing.T) {
	gw := base(t)
	payload := `{"model":"不存在的模型","messages":[{"role":"user","content":"hi"}]}`
	resp, err := client.Post(gw+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d", resp.StatusCode)
	}
}
