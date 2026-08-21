// 集群级限流：GCRA（redis_rate），同一模型的全部网关实例共享一份配额。
// Redis 故障时的降级策略由 RateLimitFailOpen 配置决定：
//
//	true  （缺省）→ 退回本地令牌桶（fail-open，保数据面可用），恢复后自动回到集群配额；
//	false → 直接拒绝（fail-closed，严格限额，宁可误杀不可超额）。
//
// 连续 Redis 错误达到阈值后进入 1s 熔断窗口：窗口内不再请求 Redis，直接走降级分支，
// 消除"每次失败都等满 OpTimeout"的 fail-slow；窗口到期后下一次 Allow 探测 Redis，
// 成功即恢复。
package cluster

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"golang.org/x/time/rate"
)

// breakerErrorThreshold 连续 Redis 错误达到该次数后进入熔断窗口。
const breakerErrorThreshold = 3

// breakerWindow 熔断窗口时长：窗口内直接走降级分支不再请求 Redis；
// 到期后下一次 Allow 充当一次探测，成功即恢复。
const breakerWindow = time.Second

// RateLimiter 集群级限流器，实现 proxy.Limiter。
type RateLimiter struct {
	rl       *redis_rate.Limiter
	key      string
	limit    redis_rate.Limit
	fallback *rate.Limiter
	timeout  time.Duration
	failOpen bool // Redis 故障时是否退回本地桶放行（false=直接拒绝）
	disabled bool // qps<=0（不合法速率）时恒拒绝，不创建退化的限速器
	errHook  func(string)

	// 熔断状态（仅 Allow 访问，原子操作，无锁）：
	// consecErr   连续错误计数；
	// openUntilNS 熔断窗口截止 unix 纳秒，0 表示未熔断。
	consecErr   atomic.Int64
	openUntilNS atomic.Int64
}

// quantizeQPS 把浮点 qps 量化为 GCRA 可表达的整数速率，并给出与分布式
// 完全一致的本地降级速率（fail-open 时的令牌桶）：
//
//   - qps >= 1：GCRA 只能表达整数的每秒速率，按 math.Round 四舍五入；
//     本地降级速率也必须用同一个量化值（而非未取整的原始 qps），否则
//     Redis 故障期间本地桶放行节奏与集群配额不一致（L5）。
//   - 0 < qps < 1：换算为「每 1/qps 时长放行 1 个」的 Period 表达，
//     本地降级速率为 1/Period（浮点意义上即 qps）。
//   - qps <= 0：不合法速率（1s/0 或负 Period 会把限速器退化/溢出），
//     返回 ok=false，调用方应拒绝而非创建退化限速器。
//
// 返回 period/perPeriod 组成 redis_rate.Limit，local 用于 rate.NewLimiter。
func quantizeQPS(qps float64) (period time.Duration, perPeriod int, local rate.Limit, ok bool) {
	if qps <= 0 {
		return 0, 0, 0, false
	}
	if qps >= 1 {
		// math.Round（四舍五入，.5 远离零）而非 int(qps+0.5)：语义显式，
		// 且对任意正数一致；钳制到 int 安全范围，避免极端值溢出导致退化。
		r := math.Round(qps)
		if r < 1 {
			r = 1
		}
		if r > math.MaxInt32 {
			r = math.MaxInt32
		}
		return time.Second, int(r), rate.Limit(r), true
	}
	// 0 < qps < 1：每 1/qps 秒放行 1 个；钳制 Period 下界避免极端小速率溢出。
	period = time.Duration(float64(time.Second) / qps)
	if period < time.Nanosecond {
		period = time.Nanosecond
	}
	// period 以纳秒计，每秒速率 = 1e9 纳秒 / period 纳秒（浮点意义上即 qps）。
	return period, 1, rate.Limit(float64(time.Second) / float64(period)), true
}

// NewRateLimiter 构造某一模型的集群级限流器。
// qps 支持小数（如 0.5 表示 2 秒 1 个）：>=1 按 math.Round 的每秒速率表达，
// 0<qps<1 换算为「每 1/qps 时长放行 1 个」；本地降级桶与分布式用同一量化速率。
// qps<=0 时调用方语义为「不限流」（装配方不会走到），此处防御性返回恒拒绝
// 的限速器，不创建速率 0/负的退化限速器。
func (m *Manager) NewRateLimiter(model string, qps float64, burst int) *RateLimiter {
	period, perPeriod, local, ok := quantizeQPS(qps)
	if !ok {
		return &RateLimiter{disabled: true, failOpen: true, errHook: m.errHook}
	}
	// 配置缺省（nil）在 ApplyDefaults 中已解析为 true；此处再兜底一次，
	// 便于测试直接构造 ClusterConfig（不经 ApplyDefaults）时行为一致。
	failOpen := true
	if m.cfg.RateLimitFailOpen != nil {
		failOpen = *m.cfg.RateLimitFailOpen
	}
	return &RateLimiter{
		rl:       redis_rate.NewLimiter(m.rdb),
		key:      m.key("ratelimit", model),
		limit:    redis_rate.Limit{Rate: perPeriod, Period: period, Burst: burst},
		fallback: rate.NewLimiter(local, burst),
		timeout:  m.cfg.OpTimeout.D(),
		failOpen: failOpen,
		errHook:  m.errHook,
	}
}

// Allow 判定是否放行。Redis 故障（含超时）时按 failOpen 走降级分支。
func (l *RateLimiter) Allow(ctx context.Context) bool {
	if l.disabled {
		return false
	}
	now := time.Now()
	if until := l.openUntilNS.Load(); until != 0 && now.UnixNano() < until {
		// 熔断窗口内：不请求 Redis（避免每次等满 OpTimeout），直接降级。
		return l.degraded()
	}

	opCtx, cancel := context.WithTimeout(ctx, l.timeout)
	res, err := l.rl.Allow(opCtx, l.key, l.limit)
	cancel()
	if err != nil {
		l.errHook("ratelimit")
		if l.consecErr.Add(1) >= breakerErrorThreshold {
			// 连续错误达到阈值：进入熔断窗口。窗口到期后下一次 Allow
			// 自然充当探测（成功则清零恢复，失败则重新进入窗口）。
			l.openUntilNS.Store(now.Add(breakerWindow).UnixNano())
		}
		return l.degraded()
	}
	// Redis 正常：清零熔断状态（连续成功即恢复）。
	l.consecErr.Store(0)
	l.openUntilNS.Store(0)
	return res.Allowed > 0
}

// degraded Redis 不可用时的降级分支：
// fail-open 走本地令牌桶放行（可用性优先），fail-closed 直接拒绝（严格限额）。
func (l *RateLimiter) degraded() bool {
	if l.failOpen {
		return l.fallback.Allow()
	}
	return false
}
