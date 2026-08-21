// 后端运行态补充单测：可用性矩阵、熔断冷却期、TTFT EWMA 与主动健康检查。
package backend

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

func mkBackend(t *testing.T, url string) *Backend {
	t.Helper()
	b, err := New(config.BackendConfig{ID: "b1", URL: url, Engine: "vllm", HealthPath: "/health"})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	return b
}

// TestAvailableMatrix 可用性矩阵：健康/隔离/冷却期的组合。
func TestAvailableMatrix(t *testing.T) {
	b := mkBackend(t, "http://127.0.0.1:1")
	now := time.Now()
	if !b.Available(now) {
		t.Fatal("初始应可用")
	}
	b.SetHealthy(false)
	if b.Available(now) {
		t.Fatal("不健康应不可用")
	}
	b.SetHealthy(true)
	b.Cordon(true)
	if b.Available(now) {
		t.Fatal("隔离应不可用")
	}
	b.Cordon(false)
	b.MarkFailure(1, 5*time.Second) // 阈值 1：立即进入冷却期
	if b.Available(now) {
		t.Fatal("冷却期内应不可用")
	}
	if !b.Ejected(now) {
		t.Fatal("冷却期内 Ejected 应为 true")
	}
	if !b.Available(now.Add(6 * time.Second)) {
		t.Fatal("冷却期过后应恢复可用")
	}
}

// TestMarkFailureCooldownExpiry 熔断冷却期自然恢复，且恢复后失败计数清零
// （后续失败重新计数，不会因冷却期结束而残留阈值状态）。
func TestMarkFailureCooldownExpiry(t *testing.T) {
	b := mkBackend(t, "http://127.0.0.1:1")
	// 阈值 3：连打 3 次失败才进入冷却期。
	for i := 0; i < 3; i++ {
		b.MarkFailure(3, 100*time.Millisecond)
	}
	if b.consecutiveFails.Load() != 0 {
		t.Fatalf("达到阈值后计数应清零: %d", b.consecutiveFails.Load())
	}
	if !b.Ejected(time.Now()) {
		t.Fatal("应处于冷却期")
	}
	time.Sleep(120 * time.Millisecond)
	if b.Ejected(time.Now()) {
		t.Fatal("冷却期应已结束")
	}
	if !b.Available(time.Now()) {
		t.Fatal("冷却期后应可用")
	}
}

// TestObserveTTFTEWMA 指数滑动均值：首样本直接采纳、后续按 0.2 平滑、
// 负值被忽略、并发 CAS 不丢样本。
func TestObserveTTFTEWMA(t *testing.T) {
	b := mkBackend(t, "http://127.0.0.1:1")
	if got := b.TTFTEWMA(); got != 0 {
		t.Fatalf("无样本时 TTFTEWMA 应为 0: %v", got)
	}
	b.ObserveTTFT(-1) // 负值忽略
	if got := b.TTFTEWMA(); got != 0 {
		t.Fatalf("负值样本不应影响 EWMA: %v", got)
	}
	b.ObserveTTFT(1.0)
	if got := b.TTFTEWMA(); got != 1.0 {
		t.Fatalf("首样本应直接采纳: %v", got)
	}
	b.ObserveTTFT(0.0)
	if got := b.TTFTEWMA(); got != 0.8 {
		t.Fatalf("EWMA 更新不符: %v (期望 0.8)", got)
	}
	// 并发上报：N 个样本后 EWMA 有限且无数据竞争（-race 下验证）。
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() { b.ObserveTTFT(0.5); done <- struct{}{} }()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if v := b.TTFTEWMA(); v < 0 || v > 1 {
		t.Fatalf("并发更新后 EWMA 越界: %v", v)
	}
}

// TestHealthCheckerCheckOne 主动健康检查：2xx 健康、5xx 不健康、连接失败不健康。
func TestHealthCheckerCheckOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()

	hc := NewHealthChecker(func() []*Backend { return nil }, time.Second, time.Second, nil)
	ok := hc.checkOne(t.Context(), mkBackend(t, srv.URL+"/health"))
	if !ok {
		t.Fatal("2xx 健康路径应返回 true")
	}
	// 5xx
	b2 := mkBackend(t, srv.URL+"/down")
	b2.HealthPath = "/down"
	if hc.checkOne(t.Context(), b2) {
		t.Fatal("5xx 应返回 false")
	}
	// 连接失败
	b3 := mkBackend(t, "http://127.0.0.1:1/health")
	if hc.checkOne(t.Context(), b3) {
		t.Fatal("连接失败应返回 false")
	}
}

// TestHealthCheckerFlipCallback 健康翻转回调：状态变化才触发（checkAll 内比较
// Healthy() 与探测结果），连续相同状态不触发。
func TestHealthCheckerFlipCallback(t *testing.T) {
	var state atomic.Int32
	state.Store(200)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(state.Load()))
	}))
	defer srv.Close()

	flips := 0
	var mu sync.Mutex
	b := mkBackend(t, srv.URL) // 初始 healthy=true
	hc := NewHealthChecker(func() []*Backend { return []*Backend{b} }, time.Second, time.Second, func(b *Backend, healthy bool) {
		mu.Lock()
		flips++
		mu.Unlock()
	})

	// checkAll 在 goroutine 内探测；用"探测请求命中数"做完成屏障，
	// 保证上一轮 goroutine 已结束再进入下一轮（避免并发翻转竞态）。
	run := func() { hc.checkAll(t.Context()) }
	waitHits := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hits.Load() >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("探测请求未达到 %d 次: %d", want, hits.Load())
	}
	waitFlips := func(want int) {
		t.Helper()
		mu.Lock()
		got := flips
		mu.Unlock()
		if got != want {
			t.Fatalf("翻转次数期望 %d，实际 %d", want, got)
		}
	}

	// 初始 healthy=true、探测 200：无翻转。
	run()
	waitHits(1)
	waitFlips(0)
	// 引擎故障（500）：true→false 翻转一次。
	state.Store(500)
	run()
	waitHits(2)
	waitFlips(1)
	// 仍故障：状态未变化，不翻转。
	run()
	waitHits(3)
	waitFlips(1)
	// 恢复（200）：false→true 再翻转一次。
	state.Store(200)
	run()
	waitHits(4)
	waitFlips(2)
}
