// 排队模块单测：容量释放唤醒、超时、队列满拒绝、上下文取消、兜底轮询。
package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignalWakesAndSucceeds(t *testing.T) {
	q := New(10, time.Second)
	var capacityFree atomic.Bool

	done := make(chan error, 1)
	go func() {
		done <- q.WaitFor(context.Background(), "m1", capacityFree.Load)
	}()

	time.Sleep(20 * time.Millisecond)
	capacityFree.Store(true)
	q.Signal()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("容量释放后应成功，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Signal 后未被唤醒")
	}
}

func TestTimeout(t *testing.T) {
	q := New(10, 50*time.Millisecond)
	start := time.Now()
	err := q.WaitFor(context.Background(), "m1", func() bool { return false })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("应超时，实际 %v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("超时过早返回")
	}
}

func TestQueueFull(t *testing.T) {
	q := New(1, time.Second)
	go q.WaitFor(context.Background(), "m1", func() bool { return false }) // 占满唯一席位
	time.Sleep(10 * time.Millisecond)
	if err := q.WaitFor(context.Background(), "m1", func() bool { return false }); !errors.Is(err, ErrFull) {
		t.Fatalf("队列满应拒绝，实际 %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	q := New(10, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.WaitFor(ctx, "m1", func() bool { return false }) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消应返回 context.Canceled，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后未返回")
	}
}

func TestFallbackPollingWithoutSignal(t *testing.T) {
	q := New(10, time.Second)
	var ready atomic.Bool
	go func() {
		time.Sleep(30 * time.Millisecond)
		ready.Store(true) // 不发 Signal，只靠兜底轮询发现
	}()
	if err := q.WaitFor(context.Background(), "m1", ready.Load); err != nil {
		t.Fatalf("兜底轮询应能发现容量，实际 %v", err)
	}
}

func TestDepthAccounting(t *testing.T) {
	q := New(10, 100*time.Millisecond)
	for i := 0; i < 3; i++ {
		go q.WaitFor(context.Background(), "m1", func() bool { return false })
	}
	time.Sleep(20 * time.Millisecond)
	if d := q.Depth(); d != 3 {
		t.Fatalf("深度期望 3，实际 %d", d)
	}
	time.Sleep(150 * time.Millisecond)
	if d := q.Depth(); d != 0 {
		t.Fatalf("全部退出后深度应为 0，实际 %d", d)
	}
}

// TestPerClassDepth 验证按队列（类别）分别核算排队并发数，退出归零后类别被清理。
func TestPerClassDepth(t *testing.T) {
	q := New(10, 100*time.Millisecond)
	for i := 0; i < 2; i++ {
		go q.WaitFor(context.Background(), "模型A", func() bool { return false })
	}
	go q.WaitFor(context.Background(), "模型B", func() bool { return false })

	time.Sleep(20 * time.Millisecond)
	depths := q.Depths()
	if depths["模型A"] != 2 || depths["模型B"] != 1 {
		t.Fatalf("按类别深度期望 A=2 B=1，实际 %v", depths)
	}
	if q.Depth() != 3 {
		t.Fatalf("全局深度期望 3，实际 %d", q.Depth())
	}

	time.Sleep(150 * time.Millisecond)
	if depths := q.Depths(); len(depths) != 0 {
		t.Fatalf("全部退出后类别表应清空，实际 %v", depths)
	}
}
