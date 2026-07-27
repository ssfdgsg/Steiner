// 集群级限流：GCRA（redis_rate），同一模型的全部网关实例共享一份配额。
// Redis 故障时退回本地令牌桶（fail-open，保数据面可用），恢复后自动回到集群配额。
package cluster

import (
	"context"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"golang.org/x/time/rate"
)

// RateLimiter 集群级限流器，实现 proxy.Limiter。
type RateLimiter struct {
	rl       *redis_rate.Limiter
	key      string
	limit    redis_rate.Limit
	fallback *rate.Limiter
	timeout  time.Duration
	errHook  func(string)
}

// NewRateLimiter 构造某一模型的集群级限流器。
// qps 支持小数（如 0.5 表示 2 秒 1 个）：整数部分 >=1 时按每秒速率表达，
// 否则换算为「每 1/qps 时长放行 1 个」。
func (m *Manager) NewRateLimiter(model string, qps float64, burst int) *RateLimiter {
	var limit redis_rate.Limit
	if qps >= 1 {
		limit = redis_rate.Limit{Rate: int(qps + 0.5), Period: time.Second, Burst: burst}
	} else {
		limit = redis_rate.Limit{Rate: 1, Period: time.Duration(float64(time.Second) / qps), Burst: burst}
	}
	return &RateLimiter{
		rl:       redis_rate.NewLimiter(m.rdb),
		key:      m.key("ratelimit", model),
		limit:    limit,
		fallback: rate.NewLimiter(rate.Limit(qps), burst),
		timeout:  m.cfg.OpTimeout.D(),
		errHook:  m.errHook,
	}
}

// Allow 判定是否放行。Redis 故障（含超时）时退回本地令牌桶。
func (l *RateLimiter) Allow(ctx context.Context) bool {
	opCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	res, err := l.rl.Allow(opCtx, l.key, l.limit)
	if err != nil {
		l.errHook("ratelimit")
		return l.fallback.Allow()
	}
	return res.Allowed > 0
}
