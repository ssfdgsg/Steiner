package metrics

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestAggregateConcurrentStress 在持续写入指标的同时反复聚合，覆盖
// Prometheus 指标采集与管理面统计并发执行的生产场景。
func TestAggregateConcurrentStress(t *testing.T) {
	const (
		writers    = 16
		readers    = 8
		iterations = 500
	)
	g := NewGateway()
	start := make(chan struct{})
	done := make(chan struct{})
	errCh := make(chan error, 1)
	var reportOnce sync.Once
	report := func(err error) {
		reportOnce.Do(func() { errCh <- err })
	}

	var readerWG sync.WaitGroup
	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				a, err := g.Aggregate()
				if err != nil {
					report(fmt.Errorf("并发 Aggregate 失败: %w", err))
					return
				}
				if a.ErrorsTotal < 0 || a.ErrorsTotal > a.RequestsTotal {
					report(fmt.Errorf("并发快照计数非法: requests=%v errors=%v", a.RequestsTotal, a.ErrorsTotal))
					return
				}
				if a.Latency.P50Ms > a.Latency.P90Ms || a.Latency.P90Ms > a.Latency.P95Ms ||
					a.Latency.P95Ms > a.Latency.P99Ms {
					report(fmt.Errorf("并发快照分位数非单调: %+v", a.Latency))
					return
				}
			}
		}()
	}

	var writerWG sync.WaitGroup
	for worker := 0; worker < writers; worker++ {
		writerWG.Add(1)
		go func(worker int) {
			defer writerWG.Done()
			<-start
			backend := fmt.Sprintf("b-%02d", worker)
			model := fmt.Sprintf("m-%02d", worker%4)
			for i := 0; i < iterations; i++ {
				code := "200"
				if i%10 == 0 {
					code = "503"
				}
				g.ReqTotal.WithLabelValues(backend, model, code).Inc()
				g.ReqDuration.WithLabelValues(backend, model).Observe(float64(i%20+1) / 1000)
			}
		}(worker)
	}

	close(start)
	writerWG.Wait()
	close(done)
	readerWG.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	a, err := g.Aggregate()
	if err != nil {
		t.Fatalf("最终 Aggregate 失败: %v", err)
	}
	wantRequests := float64(writers * iterations)
	wantErrors := float64(writers * (iterations / 10))
	if a.RequestsTotal != wantRequests || a.ErrorsTotal != wantErrors {
		t.Fatalf("压力写入后计数丢失: requests=%v/%v errors=%v/%v",
			a.RequestsTotal, wantRequests, a.ErrorsTotal, wantErrors)
	}
	if a.Latency.Count != wantRequests {
		t.Fatalf("压力写入后时延样本丢失: count=%v want=%v", a.Latency.Count, wantRequests)
	}
	if len(a.ByBackend) != writers || len(a.ByModel) != 4 {
		t.Fatalf("高基数维度聚合不完整: backends=%d models=%d", len(a.ByBackend), len(a.ByModel))
	}
}

// TestHistoryConcurrentReadWriteStress 持续采样并由多个控制台读者并发拉取，
// 验证容量上限、时间顺序和返回副本不会在压力下被破坏。
func TestHistoryConcurrentReadWriteStress(t *testing.T) {
	const (
		capacity   = 64
		iterations = 1000
		readers    = 12
	)
	g := NewGateway()
	h := NewHistory(g, time.Second, capacity)
	base := time.Now()
	h.sample(base)
	done := make(chan struct{})
	errCh := make(chan error, 1)
	var reportOnce sync.Once
	report := func(err error) {
		reportOnce.Do(func() { errCh <- err })
	}

	var readerWG sync.WaitGroup
	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				samples := h.Samples(time.Time{})
				if len(samples) > capacity {
					report(fmt.Errorf("历史缓冲越界: len=%d capacity=%d", len(samples), capacity))
					return
				}
				for j := 1; j < len(samples); j++ {
					if !samples[j-1].Time.Before(samples[j].Time) {
						report(fmt.Errorf("历史采样顺序损坏: %v >= %v", samples[j-1].Time, samples[j].Time))
						return
					}
				}
			}
		}()
	}

	for i := 1; i <= iterations; i++ {
		g.ReqTotal.WithLabelValues("b1", "m1", "200").Inc()
		g.ReqDuration.WithLabelValues("b1", "m1").Observe(0.01)
		h.sample(base.Add(time.Duration(i) * time.Second))
	}
	close(done)
	readerWG.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	got := h.Samples(time.Time{})
	if len(got) != capacity {
		t.Fatalf("压力采样后容量错误: got=%d want=%d", len(got), capacity)
	}
	if want := base.Add(time.Duration(iterations) * time.Second); !got[len(got)-1].Time.Equal(want) {
		t.Fatalf("最新采样点丢失: got=%v want=%v", got[len(got)-1].Time, want)
	}
	for _, sample := range got {
		if sample.RPS != 1 || sample.LatencyAvgMs < 9.9 || sample.LatencyAvgMs > 10.1 {
			t.Fatalf("差分采样值异常: %+v", sample)
		}
	}

	// Samples 必须返回副本；调用方修改结果不能污染环形缓冲。
	originalRPS := got[0].RPS
	got[0].RPS = -1
	if current := h.Samples(time.Time{})[0].RPS; current != originalRPS {
		t.Fatalf("调用方修改了历史缓冲: got=%v want=%v", current, originalRPS)
	}
}

// BenchmarkAggregateHighCardinality 观察管理面聚合在较多模型/后端序列下的成本。
func BenchmarkAggregateHighCardinality(b *testing.B) {
	g := NewGateway()
	for i := 0; i < 128; i++ {
		backend := fmt.Sprintf("b-%03d", i)
		model := fmt.Sprintf("m-%03d", i%32)
		g.ReqTotal.WithLabelValues(backend, model, "200").Add(100)
		g.ReqDuration.WithLabelValues(backend, model).Observe(0.05)
		g.TTFT.WithLabelValues(backend, model).Observe(0.01)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Aggregate(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHistoryFullBufferSample 覆盖默认 1440 点缓冲已满后的采样成本。
func BenchmarkHistoryFullBufferSample(b *testing.B) {
	g := NewGateway()
	h := NewHistory(g, time.Second, 1440)
	base := time.Now()
	h.sample(base)
	for i := 1; i <= h.Capacity(); i++ {
		h.sample(base.Add(time.Duration(i) * time.Second))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.sample(base.Add(time.Duration(h.Capacity()+i+1) * time.Second))
	}
}
