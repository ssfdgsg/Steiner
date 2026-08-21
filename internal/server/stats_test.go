// 控制台数据源端点测试：聚合统计、时序历史、模型路由拓扑。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/metrics"
)

func TestStatsEndpoint(t *testing.T) {
	s := newTestServer(t)
	s.gw.ReqTotal.WithLabelValues("b1", "m1", "200").Add(9)
	s.gw.ReqTotal.WithLabelValues("b1", "m1", "503").Add(1)
	s.gw.ReqDuration.WithLabelValues("b1", "m1").Observe(0.25)

	rec := do(t, s, "GET", "/admin/stats", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		UptimeSeconds float64           `json:"uptime_seconds"`
		AvgRPS        float64           `json:"avg_rps"`
		Aggregate     metrics.Aggregate `json:"aggregate"`
		Backends      backendSummary    `json:"backends"`
		Queue         map[string]any    `json:"queue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Aggregate.RequestsTotal != 10 || out.Aggregate.ErrorsTotal != 1 {
		t.Fatalf("聚合请求量口径错误: %+v", out.Aggregate)
	}
	if out.Aggregate.Latency.Count != 1 {
		t.Fatalf("时延样本数应为 1，实际 %v", out.Aggregate.Latency.Count)
	}
	if out.Backends.Total != 2 {
		t.Fatalf("后端总数应为 2，实际 %d", out.Backends.Total)
	}
	if out.UptimeSeconds <= 0 {
		t.Fatal("uptime 应为正数")
	}
	// 未启用排队时明确回 enabled=false，前端据此渲染"未启用"而非零值。
	if enabled, ok := out.Queue["enabled"].(bool); !ok || enabled {
		t.Fatalf("未配置排队时应回 enabled=false，实际 %+v", out.Queue)
	}
}

// TestStatsHistoryDisabled 未注入时序缓冲时返回 enabled=false，不报错。
func TestStatsHistoryDisabled(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/stats/history", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var out struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Enabled {
		t.Fatal("未注入缓冲时应回 enabled=false")
	}
}

func TestStatsHistoryEnabled(t *testing.T) {
	s := newTestServer(t)
	s.SetStats(metrics.NewHistory(s.gw, 5*time.Second, 100))

	rec := do(t, s, "GET", "/admin/stats/history?minutes=30", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Enabled  bool             `json:"enabled"`
		Interval float64          `json:"interval_seconds"`
		Samples  []metrics.Sample `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !out.Enabled || out.Interval != 5 {
		t.Fatalf("响应元信息错误: %+v", out)
	}
	if out.Samples == nil {
		t.Fatal("samples 应为空数组而非 null，避免前端判空分支不一致")
	}

	if rec := do(t, s, "GET", "/admin/stats/history?minutes=0", ""); rec.Code != 400 {
		t.Fatalf("非法 minutes 应 400，实际 %d", rec.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/admin/models", "")
	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out []modelView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out) != 1 || out[0].Name != "m1" {
		t.Fatalf("应返回唯一模型路由 m1，实际 %+v", out)
	}
	if out[0].Total != 2 || len(out[0].Backends) != 2 {
		t.Fatalf("模型 m1 应关联 2 个后端，实际 %+v", out[0])
	}
	if out[0].Policy == "" || out[0].Strategy == "" {
		t.Fatalf("策略与调度算法字段不应为空: %+v", out[0])
	}
}

// TestStatsEndpointsConcurrentStress 模拟控制台多标签页轮询，同时后台持续更新指标
// 与历史采样，覆盖三个新只读端点的并发安全和响应完整性。
func TestStatsEndpointsConcurrentStress(t *testing.T) {
	const (
		clients    = 12
		iterations = 100
	)
	s := newTestServer(t)
	h := metrics.NewHistory(s.gw, time.Millisecond, 64)
	s.SetStats(h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("历史采样器未及时退出")
		}
	})

	errCh := make(chan error, 1)
	var reportOnce sync.Once
	report := func(err error) {
		reportOnce.Do(func() { errCh <- err })
	}
	paths := []string{"/admin/stats", "/admin/models", "/admin/stats/history?minutes=1"}
	var wg sync.WaitGroup
	for client := 0; client < clients; client++ {
		wg.Add(1)
		go func(client int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				path := paths[(client+i)%len(paths)]
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer "+s.cfg.Server.AdminToken)
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != 200 {
					report(fmt.Errorf("%s 返回 %d: %s", path, rec.Code, rec.Body.String()))
					return
				}
				var body any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					report(fmt.Errorf("%s 返回非法 JSON: %w", path, err))
					return
				}
			}
		}(client)
	}
	for i := 0; i < clients*iterations; i++ {
		s.gw.ReqTotal.WithLabelValues("b1", "m1", "200").Inc()
		s.gw.ReqDuration.WithLabelValues("b1", "m1").Observe(0.01)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}
