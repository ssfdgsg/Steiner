# internal/queue/ — 请求排队

## 职责
集群瞬时无可用容量（候选全忙 / 全部摘除中）时，把请求**短暂排队**等待容量
释放，而非立即失败，削峰填谷。

## 行为定义（wiring 见 internal/proxy.pickWithQueue）
- 触发：调度选路返回"无可用后端"且启用了 queue；
- 有界队列（`max_depth`，默认 1024），超限立即 429（快速失败优于无界堆积）；
- 等待上限（`max_wait`，默认 5s），超时 503；
- 唤醒：事件驱动广播（请求完成释放额度、PD 转发结束、后端恢复健康）
  + 100ms 兜底轮询（防极端时序下信号丢失导致饿死）；
- 流式请求同样可排队（尚未开始响应，无副作用）；
- 排队深度暴露到 `gateway_queue_depth` 指标。

## 接口
```go
func New(maxDepth int, maxWait time.Duration) *Queue
func (q *Queue) WaitFor(ctx context.Context, retry func() bool) error // ErrFull / ErrTimeout
func (q *Queue) Signal() // 广播容量可能已释放
func (q *Queue) Depth() int64
```

## 文件
`queue.go`、`queue_test.go`（唤醒/超时/队列满/取消/兜底轮询/深度核算用例）。
