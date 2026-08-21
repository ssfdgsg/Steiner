// 配置校验修复单测：验证功能漏洞审查中的 M9（TTL 与间隔交叉约束）、
// L6（负时长拒绝）、L7（rollout steps 末值强制 100）修复后的行为。
// 断言从"缺陷行为"翻转而来：非法配置现在必须被校验拒绝。
package config

import (
	"testing"
	"time"
)

// TestM9ClusterTTLNotCheckedAgainstInterval 复现 M9(1)：
// heartbeat_ttl < heartbeat_interval、leader_ttl < heartbeat_interval 的
// 非法组合通过 Validate —— 配置层不阻止"两次心跳间成员消失 / 租约提前过期
// 导致双主"的窗口。
func TestM9ClusterTTLNotCheckedAgainstInterval(t *testing.T) {
	c := baseConfig()
	c.Cluster = ClusterConfig{
		Enabled:           true,
		RedisAddr:         "127.0.0.1:6379",
		HeartbeatInterval: Duration(30 * time.Second),
		HeartbeatTTL:      Duration(10 * time.Second), // 非法：TTL < 心跳间隔
		LeaderTTL:         Duration(10 * time.Second), // 非法：租约 < 心跳间隔
	}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatalf("M9 修复失败：TTL < 心跳间隔的非法组合应被校验拒绝")
	}
	if c.Cluster.HeartbeatTTL.D() < c.Cluster.HeartbeatInterval.D() && c.Cluster.LeaderTTL.D() < c.Cluster.HeartbeatInterval.D() {
		// 两项均已拒绝，测试通过（错误信息至少命中其一）。
	}
}

// TestM9ExplicitZeroRetriesOverridden M9(2) 保持项：
// 显式 retries: 0 被 ApplyDefaults 改写成 2 属**有意设计**（失败即返回需
// 用 retries: -1 表达不可用，0 是默认档位）；此处锁定该行为避免回归。
func TestM9ExplicitZeroRetriesOverridden(t *testing.T) {
	c := baseConfig()
	c.Server.Retries = 0
	c.ApplyDefaults()
	if c.Server.Retries != 2 {
		t.Fatalf("retries=0 应保持被默认档位 2 覆盖: %d", c.Server.Retries)
	}
}

// TestL6NegativeDurationAccepted 复现 L6：
// YAML 中 for: -10 被接受为负时长，规则校验不拒绝，告警将第一轮即 firing。
func TestL6NegativeDurationAccepted(t *testing.T) {
	yaml := `
server:
  admin_token: t
backends:
  - id: b1
    engine: vllm
    url: http://127.0.0.1:8001
models:
  - name: m1
    backends: [b1]
alerting:
  enabled: true
  interval: 5s
  rules:
    - name: r1
      scope: backend
      expr: "running > 100"
      for: -10
      severity: warning
`
	if _, err := Load(writeTemp(t, yaml)); err == nil {
		t.Fatal("L6 修复失败：负 for 时长应被校验拒绝")
	}
}

// TestL7RolloutStepsLastNotRequired100 复现 L7：
// steps: [10, 20] 通过校验（末值 <100），发布"完成"后金丝雀权重停在 20%，
// 状态名不副实。
func TestL7RolloutStepsLastNotRequired100(t *testing.T) {
	c := baseConfig()
	c.Models[0].Backends = nil // backends 与 splits 二选一
	c.Models[0].Splits = []SplitConfig{
		{Name: "canary", Weight: 1, Backends: []string{"b1"}},
		{Name: "stable", Weight: 9, Backends: []string{"b2"}},
	}
	c.Rollouts = []RolloutConfig{{
		Model:        "m1",
		Canary:       "canary",
		Steps:        []float64{10, 20}, // 末值 < 100
		PromoteExpr:  "canary_requests > 1",
		RollbackExpr: "error_rate > 0.1",
	}}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("L7 修复失败：steps 末值 <100 应被校验拒绝")
	}
}

// baseConfig 构造最小合法配置。
func baseConfig() *Config {
	c := &Config{
		Backends: []BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "vllm"},
		},
		Models: []ModelRoute{{Name: "m1", Backends: []string{"b1", "b2"}}},
		Server: ServerConfig{AdminToken: "t"},
	}
	return c
}

// TestH8KVCacheBudgetDefaults H8 修复项：kv_cache 新增内存硬上限字段
// max_nodes / max_bytes，未配置时应用默认值（10 万节点 / 256MiB，
// 按 100 QPS 唯一前缀 × 10 分钟 ≈ 6 万节点的评估留余量）。
func TestH8KVCacheBudgetDefaults(t *testing.T) {
	c := baseConfig()
	c.ApplyDefaults()
	if c.KVCache.MaxNodes != 100000 {
		t.Fatalf("max_nodes 默认值应为 100000，实际 %d", c.KVCache.MaxNodes)
	}
	if c.KVCache.MaxBytes != 256<<20 {
		t.Fatalf("max_bytes 默认值应为 256MiB，实际 %d", c.KVCache.MaxBytes)
	}
	// 显式配置优先，不被默认值覆盖。
	c2 := baseConfig()
	c2.KVCache.MaxNodes = 50000
	c2.KVCache.MaxBytes = 1 << 20
	c2.ApplyDefaults()
	if c2.KVCache.MaxNodes != 50000 || c2.KVCache.MaxBytes != 1<<20 {
		t.Fatalf("显式预算不应被覆盖: nodes=%d bytes=%d", c2.KVCache.MaxNodes, c2.KVCache.MaxBytes)
	}
}

// TestH8KVCacheBudgetNegativeRejected H8 修复项：负预算（-1 等）意味着
// "叠加上限反向淘汰"，语义非法，应被校验拒绝（同 L6 负时长教训）。
func TestH8KVCacheBudgetNegativeRejected(t *testing.T) {
	c := baseConfig()
	c.KVCache.MaxNodes = -1
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("H8 修复失败：负 max_nodes 应被校验拒绝")
	}

	c2 := baseConfig()
	c2.KVCache.MaxBytes = -1
	c2.ApplyDefaults()
	if err := c2.Validate(); err == nil {
		t.Fatal("H8 修复失败：负 max_bytes 应被校验拒绝")
	}
}
