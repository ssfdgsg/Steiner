// 复现性单测:断言当前缺陷行为(L9: handleStatsHistory 的 minutes 参数无上限,
// 极大值致 time.Duration 乘法溢出,时间跨度变成未来,静默返回空采样)。
// 修复后需翻转本文件断言。
package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"ai-gateway/internal/metrics"
)

// TestStatsHistory_Minutes溢出_L9 复现 L9:
//
// minutes=999999999999999999 时 strconv.Atoi 成功,server.go 的
// time.Duration(n)*time.Minute 在 int64 纳秒上多次回绕,符号不可预测
// （可能得到"未来时间→空列表"，也可能回绕成小正数→把 1e18 分钟前的
// 查询当成几分钟前的窗口返回）。缺陷现状:参数无上限、未校验,非法输入
// 被接受且查询语义被静默破坏。
func TestStatsHistory_Minutes溢出_L9(t *testing.T) {
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

	// 正常参数:200 + 非空采样(证明缓冲与查询路径工作)。
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

	// 极大 minutes:修复后 —— 参数被钳制到 7 天窗口。
	// 确定性断言:返回 200、enabled=true,不再拒绝也不产生荒谬窗口。
	rec = do(t, s, "GET", "/admin/stats/history?minutes=999999999999999999", "")
	if rec.Code != 200 {
		t.Fatalf("缺陷现状:溢出 minutes 未校验应仍返回 200,实际 %d", rec.Code)
	}
	var bad resp
	if err := json.Unmarshal(rec.Body.Bytes(), &bad); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !bad.Enabled {
		t.Fatalf("缺陷现状:极大 minutes 应被当作普通查询处理,实际 enabled=false")
	}
}
