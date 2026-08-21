// 修复后单测：L9（stats/history minutes 无上限）的钳制行为。
//
// 修复内容：handleStatsHistory 对 minutes 钳制到 [1, 10080]（7 天），
// 消除 time.Duration(n)*time.Minute 的 int64 溢出回绕（超大 n 可能溢出为正
// 或负，静默破坏时间窗口语义——修复前同一荒谬参数返回"全部采样"或"空列表"
// 取决于溢出符号）。
//
// 断言从复现文件翻转而来：两个修复前行为相反的巨大取值，修复后行为一致
// （200 + 包含全部缓冲采样，窗口钳制为 7 天）。
package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"ai-gateway/internal/metrics"
)

// TestStatsHistory_Minutes钳制_L9 验证钳制：任意极大 minutes 都按 7 天窗口
// 处理（缓冲内的采样全部命中，且不再出现"空列表/全部采样"的符号分裂）。
func TestStatsHistory_Minutes钳制_L9(t *testing.T) {
	s := newTestServer(t)
	h := metrics.NewHistory(s.gw, 10*time.Millisecond, 50)
	s.SetStats(h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	time.Sleep(80 * time.Millisecond) // 首个采样为差分基线,之后才开始入缓冲

	type resp struct {
		Enabled bool             `json:"enabled"`
		Samples []metrics.Sample `json:"samples"`
	}

	// 基线:正常参数 → 200 + 非空采样(证明缓冲与查询路径工作)。
	rec := do(t, s, "GET", "/admin/stats/history?minutes=60", "")
	if rec.Code != 200 {
		t.Fatalf("正常参数应 200,实际 %d", rec.Code)
	}
	var ok resp
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !ok.Enabled || len(ok.Samples) == 0 {
		t.Fatalf("正常参数应返回非空采样: enabled=%v samples=%d", ok.Enabled, len(ok.Samples))
	}

	// 修复前行为分裂的两个取值:修复后钳制为 7 天窗口,全部缓冲采样命中,
	// 数量 ≥ 正常参数查询(同窗口),且绝不再出现"空列表"。
	for _, n := range []string{"999999999999999999", "100000000000000000"} {
		rec = do(t, s, "GET", "/admin/stats/history?minutes="+n, "")
		if rec.Code != 200 {
			t.Fatalf("钳制后极大 minutes=%s 应仍 200,实际 %d", n, rec.Code)
		}
		var r resp
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !r.Enabled {
			t.Fatalf("钳制后极大 minutes=%s 应 enabled=true", n)
		}
		if len(r.Samples) < len(ok.Samples) {
			t.Fatalf("钳制后极大 minutes=%s 应返回全部缓冲采样(≥ 正常查询 %d 条),实际 %d 条",
				n, len(ok.Samples), len(r.Samples))
		}
	}
}
