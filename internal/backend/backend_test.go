// 后端状态机单测：并发额度、被动摘除与恢复。
package backend

import (
	"testing"
	"time"

	"ai-gateway/internal/config"
)

func newTestBackend(t *testing.T, maxConc int) *Backend {
	t.Helper()
	b, err := New(config.BackendConfig{
		ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm", Weight: 1, MaxConcurrency: maxConc,
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	return b
}

func TestTryAcquireLimit(t *testing.T) {
	b := newTestBackend(t, 2)
	if !b.TryAcquire() || !b.TryAcquire() {
		t.Fatal("前两次占用应成功")
	}
	if b.TryAcquire() {
		t.Fatal("超过上限的占用应失败")
	}
	b.Release()
	if !b.TryAcquire() {
		t.Fatal("释放后应可再次占用")
	}
}

func TestUnlimitedConcurrency(t *testing.T) {
	b := newTestBackend(t, 0)
	for i := 0; i < 1000; i++ {
		if !b.TryAcquire() {
			t.Fatal("无上限时不应失败")
		}
	}
}

func TestMarkFailureEjects(t *testing.T) {
	b := newTestBackend(t, 0)
	if !b.Available(time.Now()) {
		t.Fatal("初始应可用")
	}
	b.MarkFailure(2, 50*time.Millisecond)
	if !b.Available(time.Now()) {
		t.Fatal("未达阈值不应摘除")
	}
	b.MarkFailure(2, 50*time.Millisecond)
	if b.Available(time.Now()) {
		t.Fatal("达到阈值应被动摘除")
	}
	time.Sleep(60 * time.Millisecond)
	if !b.Available(time.Now()) {
		t.Fatal("冷却期结束应自动恢复")
	}
}

func TestMarkSuccessResets(t *testing.T) {
	b := newTestBackend(t, 0)
	b.MarkFailure(3, time.Minute)
	b.MarkFailure(3, time.Minute)
	b.MarkSuccess() // 清零连续失败
	b.MarkFailure(3, time.Minute)
	if !b.Available(time.Now()) {
		t.Fatal("成功后失败计数应清零，不应被摘除")
	}
}

func TestCordon(t *testing.T) {
	b := newTestBackend(t, 0)
	b.Cordon(true)
	if b.Available(time.Now()) {
		t.Fatal("人工隔离后不应可用")
	}
	b.Cordon(false)
	if !b.Available(time.Now()) {
		t.Fatal("解除隔离后应恢复")
	}
}

func TestBadURL(t *testing.T) {
	if _, err := New(config.BackendConfig{ID: "x", URL: "://bad", Engine: "vllm"}); err == nil {
		t.Fatal("非法 URL 应报错")
	}
}

// TestNewClampsNonPositiveWeight 验证 M15：注册/更新时 Weight<=0 钳制为 1，
// 保证加权调度权重和恒 >0、任何后端至少可被选中（与 ApplyDefaults 语义一致）。
func TestNewClampsNonPositiveWeight(t *testing.T) {
	for _, w := range []float64{0, -5} {
		b, err := New(config.BackendConfig{ID: "w", URL: "http://127.0.0.1:9", Engine: "vllm", Weight: w})
		if err != nil {
			t.Fatalf("weight=%v 构造失败: %v", w, err)
		}
		if b.Weight != 1 {
			t.Fatalf("weight=%v 应钳制为 1，实际 %v", w, b.Weight)
		}
	}
	// 正权重原样保留。
	b, err := New(config.BackendConfig{ID: "w2", URL: "http://127.0.0.1:10", Engine: "vllm", Weight: 3})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if b.Weight != 3 {
		t.Fatalf("正权重应保留，实际 %v", b.Weight)
	}
}
