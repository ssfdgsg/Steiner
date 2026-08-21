// Package queue 实现请求排队与准入：集群瞬时无可用容量（候选全忙 /
// 全部摘除中）时，把请求短暂挂起等待容量释放，而非立即失败。
//
// 语义：
//   - 有界队列（MaxDepth），超限立即拒绝（快速失败优于无界堆积）；
//   - 等待上限（MaxWait），超时返回超时错误；
//   - 事件驱动唤醒（Signal：请求完成释放槽位、指标快照刷新）+ 100ms 兜底轮询，
//     防止信号丢失导致的饿死。
package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrFull 队列已满。
var ErrFull = errors.New("排队队列已满")

// ErrTimeout 排队等待超时。
var ErrTimeout = errors.New("排队等待超时")

// Queue 容量等待队列。深度按类别（通常为模型路由名）分别核算，
// 便于观测每个队列的排队并发数；容量上限（MaxDepth）仍为全局约束。
type Queue struct {
	maxDepth int
	maxWait  time.Duration

	depth atomic.Int64

	// classMu 保护按类别的深度表；排队是慢路径，锁竞争可忽略。
	classMu sync.Mutex
	classes map[string]int64

	// waiters 等待者计数：WaitFor 阻塞前 +1、退出时 -1。
	// Signal 依据 waiters 而非队列深度决定是否广播——深度在等待者进出间
	// 可能瞬时归零（如上一等待者刚拿到容量退出），若只看深度会在这类
	// 窗口内丢信号，等待者只能等 100ms 兜底轮询，放大排队时延。
	waiters atomic.Int64

	mu sync.Mutex
	ch chan struct{} // 广播通道：Signal 时关闭并更换
}

// New 构造队列。
func New(maxDepth int, maxWait time.Duration) *Queue {
	return &Queue{
		maxDepth: maxDepth, maxWait: maxWait,
		classes: map[string]int64{}, ch: make(chan struct{}),
	}
}

// Depth 当前全局排队深度（观测用）。
func (q *Queue) Depth() int64 { return q.depth.Load() }

// Depths 各类别当前排队深度快照（观测用，键为入队时传入的 class）。
func (q *Queue) Depths() map[string]int64 {
	q.classMu.Lock()
	defer q.classMu.Unlock()
	out := make(map[string]int64, len(q.classes))
	for k, v := range q.classes {
		out[k] = v
	}
	return out
}

// enterClass / leaveClass 维护按类别深度；归零即删除，避免标签集合无界增长。
func (q *Queue) enterClass(class string) {
	q.classMu.Lock()
	q.classes[class]++
	q.classMu.Unlock()
}

func (q *Queue) leaveClass(class string) {
	q.classMu.Lock()
	if q.classes[class]--; q.classes[class] <= 0 {
		delete(q.classes, class)
	}
	q.classMu.Unlock()
}

// Signal 广播容量可能已释放，唤醒全部等待者。
// 在请求完成释放并发额度、后端恢复健康等时机调用。
// 无人排队时直接返回：调用方（每个请求完成路径）无需感知队列状态，
// 空队列的高频 Signal 不再触发加锁与 channel 重建。
// 判定依据是等待者计数而非队列深度：深度会在等待者进出窗口内瞬时归零
// （上一等待者刚退出），此时仍有等待者正在阻塞，信号不能被丢弃。
func (q *Queue) Signal() {
	if q.waiters.Load() == 0 {
		return
	}
	q.mu.Lock()
	close(q.ch)
	q.ch = make(chan struct{})
	q.mu.Unlock()
}

// waitCh 取当前广播通道。
func (q *Queue) waitCh() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ch
}

// WaitFor 排队等待直到 retry 返回 true（拿到了容量）或超时/取消。
// class 为队列类别（通常传模型路由名），用于按队列核算排队并发数；
// retry 由调用方提供，通常是"再次尝试选路"；返回 nil 表示 retry 已成功。
func (q *Queue) WaitFor(ctx context.Context, class string, retry func() bool) error {
	if int(q.depth.Add(1)) > q.maxDepth {
		q.depth.Add(-1)
		return ErrFull
	}
	defer q.depth.Add(-1)
	q.enterClass(class)
	defer q.leaveClass(class)

	deadline := time.NewTimer(q.maxWait)
	defer deadline.Stop()
	// 兜底轮询：唤醒信号可能在极端时序下丢失（先 Signal 后取通道）。
	fallback := time.NewTicker(100 * time.Millisecond)
	defer fallback.Stop()

	// 阻塞前登记为等待者：Signal 依据 waiters 决定是否广播。
	// 必须先于取通道（waitCh）完成——若 Signal 先于本语句发生，
	// waiters 仍为 0 而无人需要被唤醒，语义正确；反之 Signal 看到
	// waiters>0 必然广播，等待者随后取到的新通道不会吞掉信号。
	q.waiters.Add(1)
	defer q.waiters.Add(-1)

	for {
		ch := q.waitCh()
		select {
		case <-ch:
		case <-fallback.C:
		case <-deadline.C:
			return ErrTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
		if retry() {
			return nil
		}
	}
}
