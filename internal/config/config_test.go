// 配置模块单测：加载、默认值填充与静态校验。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTemp 把配置文本写入临时文件。
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return p
}

const minimalYAML = `
backends:
  - id: b1
    engine: vllm
    url: http://127.0.0.1:8001
models:
  - name: m1
    backends: [b1]
`

func TestLoadMinimalAndDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("默认监听地址不符: %s", cfg.Server.Listen)
	}
	if cfg.Server.Retries != 2 {
		t.Fatalf("默认重试次数不符: %d", cfg.Server.Retries)
	}
	if cfg.Metrics.ScrapeInterval.D() != 2*time.Second {
		t.Fatalf("默认抓取周期不符: %v", cfg.Metrics.ScrapeInterval.D())
	}
	if _, ok := cfg.Policies[DefaultPolicyName]; !ok {
		t.Fatal("应自动注入内置 default 策略")
	}
	m := cfg.Models[0]
	if m.Strategy != "expression" || m.Policy != DefaultPolicyName {
		t.Fatalf("模型路由默认策略不符: %+v", m)
	}
	b := cfg.Backends[0]
	if b.MetricsPath != "/metrics" || b.HealthPath != "/health" || b.Weight != 1 {
		t.Fatalf("后端默认值不符: %+v", b)
	}
	if cfg.Session.TTL.D() != 10*time.Minute || cfg.Queue.MaxDepth != 1024 {
		t.Fatalf("session/queue 默认值不符: %+v %+v", cfg.Session, cfg.Queue)
	}
}

func TestDurationFormats(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML+`
server:
  upstream_connect_timeout: 1500ms
metrics:
  scrape_interval: 3
`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Server.UpstreamConnectTimeout.D() != 1500*time.Millisecond {
		t.Fatalf("字符串时长解析失败: %v", cfg.Server.UpstreamConnectTimeout.D())
	}
	if cfg.Metrics.ScrapeInterval.D() != 3*time.Second {
		t.Fatalf("数字时长（秒）解析失败: %v", cfg.Metrics.ScrapeInterval.D())
	}
}

// mustFail 断言配置加载失败且错误信息包含关键字。
func mustFail(t *testing.T, yaml, keyword string) {
	t.Helper()
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatalf("应校验失败（%s）", keyword)
	}
	if !strings.Contains(err.Error(), keyword) {
		t.Fatalf("错误信息应包含 %q，实际: %v", keyword, err)
	}
}

func TestValidateDuplicateBackend(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: vllm, url: "http://x:1" }
  - { id: b1, engine: vllm, url: "http://x:2" }
models:
  - { name: m1, backends: [b1] }
`, "重复")
}

func TestValidateBadEngine(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: tgi, url: "http://x:1" }
models:
  - { name: m1, backends: [b1] }
`, "engine")
}

func TestValidateMissingBackendRef(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: vllm, url: "http://x:1" }
models:
  - { name: m1, backends: [nope] }
`, "不存在的后端")
}

func TestValidateMissingPolicyRef(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: vllm, url: "http://x:1" }
models:
  - { name: m1, backends: [b1], strategy: expression, policy: nope }
`, "策略")
}

func TestValidatePDGroupRefs(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: vllm, url: "http://x:1" }
pd_groups:
  - { name: g1, prefill: [b1], decode: [nope] }
models:
  - { name: m1, pd_group: g1 }
`, "pd_group")
}

func TestValidatePDLinkOutsideGroup(t *testing.T) {
	mustFail(t, `
backends:
  - { id: p1, engine: vllm, url: "http://x:1" }
  - { id: d1, engine: vllm, url: "http://x:2" }
  - { id: other, engine: vllm, url: "http://x:3" }
pd_groups:
  - name: g1
    prefill: [p1]
    decode: [d1]
    nccl_links:
      - { prefill: p1, decode: other }
models:
  - { name: m1, pd_group: g1 }
`, "组外")
}

func TestValidateBackendsAndPDGroupExclusive(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: vllm, url: "http://x:1" }
pd_groups:
  - { name: g1, prefill: [b1], decode: [b1] }
models:
  - { name: m1, backends: [b1], pd_group: g1 }
`, "只能选其一")
}

func TestValidateNoModels(t *testing.T) {
	mustFail(t, `
backends:
  - { id: b1, engine: vllm, url: "http://x:1" }
`, "模型路由")
}

// TestPolicyPresetExpansion 配置中引用预设时，加载期展开为预设表达式。
func TestPolicyPresetExpansion(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML+`
policies:
  default:
    preset: latency_first
`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	p := cfg.Policies["default"]
	if p.Score == "" || p.Filter == "" {
		t.Fatalf("预设应展开为表达式: %+v", p)
	}
	if !strings.Contains(p.Score, "ttft_ewma") {
		t.Fatalf("latency_first 的 score 应含 ttft_ewma: %s", p.Score)
	}
}

// TestPolicyPresetUnknown 未知预设名在加载期报错并提示可选值。
func TestPolicyPresetUnknown(t *testing.T) {
	mustFail(t, minimalYAML+`
policies:
  default:
    preset: 不存在的方案
`, "不存在的预设")
}

// TestPolicyPresetConflict preset 与手写表达式不能同时给出。
func TestPolicyPresetConflict(t *testing.T) {
	mustFail(t, minimalYAML+`
policies:
  default:
    preset: balanced
    score: "running * 1.0"
`, "二选一")
}
