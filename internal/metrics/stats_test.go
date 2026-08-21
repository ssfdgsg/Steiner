package metrics

import (
	"context"
	"testing"
	"time"
)

// TestAggregateFromRegistry 校验聚合口径：请求总数、错误判定（>=400 与非数字码）、
// 时延均值与分位数、按模型/后端维度拆分。
func TestAggregateFromRegistry(t *testing.T) {
	g := NewGateway()
	g.ReqTotal.WithLabelValues("b1", "m1", "200").Add(8)
	g.ReqTotal.WithLabelValues("b1", "m1", "500").Add(2)
	g.ReqTotal.WithLabelValues("b2", "m2", "200").Add(5)
	g.ReqTotal.WithLabelValues("b2", "m2", "error").Add(1)
	g.Retries.Add(3)
	g.RateLimited.WithLabelValues("m1").Add(4)
	for i := 0; i < 10; i++ {
		g.ReqDuration.WithLabelValues("b1", "m1").Observe(0.1)
	}
	g.ReqDuration.WithLabelValues("b2", "m2").Observe(0.4)
	g.TTFT.WithLabelValues("b1", "m1").Observe(0.02)

	a, err := g.Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if a.RequestsTotal != 16 {
		t.Fatalf("请求总数应为 16，实际 %v", a.RequestsTotal)
	}
	if a.ErrorsTotal != 3 {
		t.Fatalf("错误数应为 3（500 与 error），实际 %v", a.ErrorsTotal)
	}
	if a.ErrorRate <= 0.18 || a.ErrorRate >= 0.19 {
		t.Fatalf("错误率应约 0.1875，实际 %v", a.ErrorRate)
	}
	if a.Retries != 3 || a.RateLimited != 4 {
		t.Fatalf("重试/限流计数错误: %v %v", a.Retries, a.RateLimited)
	}
	if a.Latency.Count != 11 {
		t.Fatalf("时延样本数应为 11，实际 %v", a.Latency.Count)
	}
	// (10*0.1 + 0.4) / 11 * 1000 ≈ 127.3ms
	if a.Latency.AvgMs < 120 || a.Latency.AvgMs > 135 {
		t.Fatalf("时延均值偏离预期: %v", a.Latency.AvgMs)
	}
	if a.Latency.P95Ms < a.Latency.P50Ms {
		t.Fatalf("P95 不应小于 P50: %v < %v", a.Latency.P95Ms, a.Latency.P50Ms)
	}
	if a.TTFT.Count != 1 {
		t.Fatalf("TTFT 样本数应为 1，实际 %v", a.TTFT.Count)
	}
	if len(a.ByModel) != 2 || a.ByModel[0].Name != "m1" || a.ByModel[0].Requests != 10 {
		t.Fatalf("按模型聚合应按请求量降序: %+v", a.ByModel)
	}
	if a.ByModel[0].Errors != 2 {
		t.Fatalf("m1 错误数应为 2，实际 %v", a.ByModel[0].Errors)
	}
	if len(a.ByBackend) != 2 || a.ByBackend[0].Name != "b1" {
		t.Fatalf("按后端聚合结果错误: %+v", a.ByBackend)
	}
	if a.ByCode["200"] != 13 {
		t.Fatalf("按状态码聚合错误: %+v", a.ByCode)
	}
}

// TestHistorySampling 校验差分语义：首个采样点仅作基线不入缓冲，
// 第二个点给出区间 RPS 与错误率，并带上探针提供的运行态。
func TestHistorySampling(t *testing.T) {
	g := NewGateway()
	h := NewHistory(g, time.Second, 3)
	h.SetProbe(func() RuntimeSnapshot {
		return RuntimeSnapshot{QueueDepth: 7, BackendsHealthy: 2, BackendsTotal: 3, KVUsage: 0.5}
	})

	base := time.Now()
	h.sample(base)
	if got := len(h.Samples(time.Time{})); got != 0 {
		t.Fatalf("首次采样应仅作基线，实际入缓冲 %d 点", got)
	}
	g.ReqTotal.WithLabelValues("b1", "m1", "200").Add(18)
	g.ReqTotal.WithLabelValues("b1", "m1", "500").Add(2)
	g.ReqDuration.WithLabelValues("b1", "m1").Observe(0.2)
	h.sample(base.Add(10 * time.Second))

	got := h.Samples(time.Time{})
	if len(got) != 1 {
		t.Fatalf("应有 1 个采样点，实际 %d", len(got))
	}
	s := got[0]
	if s.RPS != 2 {
		t.Fatalf("RPS 应为 20/10s=2，实际 %v", s.RPS)
	}
	if s.ErrorRate != 0.1 {
		t.Fatalf("错误率应为 0.1，实际 %v", s.ErrorRate)
	}
	if s.QueueDepth != 7 || s.BackendsHealthy != 2 || s.BackendsTotal != 3 {
		t.Fatalf("探针运行态未写入: %+v", s)
	}
	if s.LatencyAvgMs < 190 || s.LatencyAvgMs > 210 {
		t.Fatalf("区间时延均值应约 200ms，实际 %v", s.LatencyAvgMs)
	}
}

// TestHistoryRingBufferEvicts 校验容量上限：超出容量后丢弃最旧的点。
func TestHistoryRingBufferEvicts(t *testing.T) {
	g := NewGateway()
	h := NewHistory(g, time.Second, 2)
	base := time.Now()
	for i := 0; i < 5; i++ {
		h.sample(base.Add(time.Duration(i) * time.Second))
	}
	got := h.Samples(time.Time{})
	if len(got) != 2 {
		t.Fatalf("缓冲应被裁剪到 2 点，实际 %d", len(got))
	}
	if !got[0].Time.Before(got[1].Time) {
		t.Fatalf("采样点应按时间升序: %v %v", got[0].Time, got[1].Time)
	}
}

// TestHistoryRunStops 校验 Run 随 ctx 取消退出。
func TestHistoryRunStops(t *testing.T) {
	h := NewHistory(NewGateway(), 10*time.Millisecond, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未随 ctx 取消退出")
	}
}
