// 主动健康检查：周期性 GET 各后端的 health 路径，2xx 视为健康。
// 与被动摘除（MarkFailure）互补：主动检查发现进程级故障，
// 被动摘除发现请求级故障。
package backend

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// HealthChecker 主动健康检查器。
type HealthChecker struct {
	backends func() []*Backend
	interval time.Duration
	client   *http.Client
	// onChange 健康状态翻转时回调（用于打点/日志），可为 nil。
	onChange func(b *Backend, healthy bool)
}

// NewHealthChecker 构造健康检查器；backends 为活视图函数（通常传
// Registry.All），动态注册的后端在下一个检查节拍自动纳入。
func NewHealthChecker(backends func() []*Backend, interval, timeout time.Duration, onChange func(*Backend, bool)) *HealthChecker {
	return &HealthChecker{
		backends: backends,
		interval: interval,
		client:   &http.Client{Timeout: timeout},
		onChange: onChange,
	}
}

// Run 阻塞运行直到 ctx 取消。启动时立即做一轮检查。
func (h *HealthChecker) Run(ctx context.Context) {
	h.checkAll(ctx)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkAll(ctx)
		}
	}
}

func (h *HealthChecker) checkAll(ctx context.Context) {
	for _, b := range h.backends() {
		b := b
		go func() {
			ok := h.checkOne(ctx, b)
			if b.Healthy() != ok {
				slog.Warn("后端健康状态变更", "backend", b.ID, "healthy", ok)
				if h.onChange != nil {
					h.onChange(b, ok)
				}
			}
			b.SetHealthy(ok)
		}()
	}
}

func (h *HealthChecker) checkOne(ctx context.Context, b *Backend) bool {
	u := *b.URL
	u.Path = b.HealthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 读尽响应体使 keep-alive 连接回池复用，避免每个探测周期新建 TCP/TLS 连接。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
