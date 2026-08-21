// 复现性单测（H11 修复后翻转/拆分）：
//   - fail_open 默认（RateLimitFailOpen 缺省 true，可用性优先）：Redis 故障后
//     每实例退回本地令牌桶、各自放行满额 burst 是**既定语义**而非缺陷
//     （故障期间集群总量可能 N× 超额，属 fail-open 的固有代价）；
//   - fail_closed（RateLimitFailOpen=false，严格限额）：Redis 故障直接拒绝，不超额；
//   - fail-slow 消除：连续 Redis 错误 ≥3 次进入熔断窗口，窗口内不再请求 Redis
//     （用"黑洞"Redis 计数验证，确定性断言），放行路径不再每次等满 OpTimeout。
package cluster

import (
	"context"
	"fmt"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// TestRateLimiter_FailOpen默认_Redis故障本地桶放行_H11 翻转原 H11 复现断言：
// RateLimitFailOpen 缺省 true（可用性优先）——Redis 故障后每实例退回本地令牌桶、
// 各自放行满额 burst 是既定 fail-open 语义（修复前把它当缺陷复现，修复后翻转）；
// 本地桶耗尽后即拒绝，放行上限恰是"满额本地桶"而非无限放行。
func TestRateLimiter_FailOpen默认_Redis故障本地桶放行_H11(t *testing.T) {
	mr := miniredis.RunT(t)
	m1 := newManager(t, mr, "gw-1")
	m2 := newManager(t, mr, "gw-2")
	l1 := m1.NewRateLimiter("m1", 1, 3)
	l2 := m2.NewRateLimiter("m1", 1, 3)

	// Redis 故障（fail-open 触发点）。
	mr.Close()
	ctx := context.Background()

	allowed1, allowed2 := 0, 0
	for i := 0; i < 3; i++ {
		if l1.Allow(ctx) {
			allowed1++
		}
		if l2.Allow(ctx) {
			allowed2++
		}
	}
	// 既定 fail-open 语义：每实例本地桶各放行满额 burst=3。
	if allowed1 != 3 || allowed2 != 3 {
		t.Fatalf("fail-open 语义：Redis 故障期间每实例本地桶应放行满额 burst=3，实际 gw-1=%d gw-2=%d", allowed1, allowed2)
	}
	// 本地满额桶耗尽后即拒绝：证明放行上限恰是"满额本地桶"，而非无限放行。
	if l1.Allow(ctx) || l2.Allow(ctx) {
		t.Fatal("本地满额桶 burst=3 耗尽后应拒绝")
	}
}

// TestRateLimiter_FailClosed_Redis故障直接拒绝_H11 新增 fail_closed 分支：
// RateLimitFailOpen=false（严格限额）时 Redis 故障 → 直接拒绝，不回退本地桶、
// 不产生任何超额放行。
func TestRateLimiter_FailClosed_Redis故障直接拒绝_H11(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := testConfig(mr.Addr(), "gw-1")
	failClosed := false
	cfg.RateLimitFailOpen = &failClosed
	m, err := New(cfg, ":8080", nil)
	if err != nil {
		t.Fatalf("构造管理器失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	l := m.NewRateLimiter("m1", 1, 3)

	// Redis 故障后连续请求：全部直接拒绝（含熔断窗口内的降级分支），0 超额。
	mr.Close()
	for i := 0; i < 6; i++ {
		if l.Allow(context.Background()) {
			t.Fatalf("fail-closed：Redis 故障时应直接拒绝，第 %d 次被放行", i+1)
		}
	}
}

// startStalledRedis 启动"黑洞"TCP 服务：接受连接并读取命令，但从不响应，
// 模拟 Redis 挂起——每次 Redis 操作会阻塞到 OpTimeout 才返回错误。
// 返回监听地址与"已收到命令数"计数器（熔断后命令数应停止增长）。
func startStalledRedis(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动黑洞服务失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var cmds atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						cmds.Add(1)
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), &cmds
}

// TestRateLimiter_熔断窗口消除fail_slow_H11 验证 fail-slow 消除：
// Redis 挂起时（每次操作等满 OpTimeout），连续 3 次错误后进入 1s 熔断窗口，
// 窗口内 Allow 不再请求 Redis、立即走降级分支。修复前每次 Allow 都等满
// OpTimeout（fail-slow）；修复后仅前 3 次等待。
func TestRateLimiter_熔断窗口消除fail_slow_H11(t *testing.T) {
	addr, cmds := startStalledRedis(t)
	const opTimeout = 50 * time.Millisecond
	rdb := redis.NewClient(&redis.Options{
		Addr:            addr,
		DialTimeout:     time.Second,
		ReadTimeout:     opTimeout,
		WriteTimeout:    opTimeout,
		MaxRetries:      0,
		PoolSize:        1,
		Protocol:        2, // 避免 RESP3 HELLO 等握手命令干扰命令计数
		DisableIdentity: true,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	l := &RateLimiter{
		rl:       redis_rate.NewLimiter(rdb),
		key:      "test:ratelimit:m1",
		limit:    redis_rate.Limit{Rate: 1000, Period: time.Second, Burst: 100},
		fallback: rate.NewLimiter(rate.Limit(1000), 100),
		timeout:  opTimeout,
		failOpen: true,
		errHook:  func(string) {},
	}

	ctx := context.Background()
	// 前 3 次：Redis 每次等满 OpTimeout 后失败，恰好触发熔断（连续 3 错）。
	for i := 0; i < 3; i++ {
		if !l.Allow(ctx) {
			t.Fatalf("fail-open：熔断前本地桶应放行，第 %d 次被拒", i+1)
		}
	}
	if got := cmds.Load(); got != 3 {
		t.Fatalf("触发熔断前应恰好收到 3 条 Redis 命令，实际 %d", got)
	}

	// 熔断窗口内连续放行多次：不得再请求 Redis（命令数不增长），
	// 且总耗时远小于 N×OpTimeout（修复前 5 次 × 50ms = 250ms）。
	before := cmds.Load()
	start := time.Now()
	allowed := 0
	for i := 0; i < 5; i++ {
		if l.Allow(ctx) {
			allowed++
		}
	}
	elapsed := time.Since(start)
	if after := cmds.Load(); after != before {
		t.Fatalf("熔断窗口内不应再请求 Redis：命令数 %d → %d", before, after)
	}
	if allowed != 5 {
		t.Fatalf("fail-open：熔断窗口内本地桶应全部放行，实际 %d/5", allowed)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("fail-slow 未消除：熔断窗口内 5 次放行耗时 %v（应远小于 5×OpTimeout，证明未每次等满）", elapsed)
	}
}

// TestQuantizeQPS_L5 验证 qps 量化一致性（L5）：分布式 GCRA 只能表达整数速率，
// qps>=1 按 math.Round 四舍五入；0<qps<1 用「每 1/qps 秒 1 个」的 Period 表达；
// 本地降级速率必须与分布式用同一量化值，且 qps<=0 不产生退化限速器。
func TestQuantizeQPS_L5(t *testing.T) {
	cases := []struct {
		qps       float64
		period    time.Duration
		perPeriod int
		local     float64
		ok        bool
	}{
		{0.4, 2500 * time.Millisecond, 1, 0.4, true},
		{0.6, 1666666666 * time.Nanosecond, 1, 0.6, true}, // 1s/0.6 截断到纳秒
		{0.9, 1111111111 * time.Nanosecond, 1, 0.9, true},
		{1.0, time.Second, 1, 1.0, true},
		// 1.4 / 1.6：修复前 fallback 用未取整的原始 qps（1.4/s、1.6/s），
		// 与分布式 round 值（1/s、2/s）不一致；修复后本地与分布式同速率。
		{1.4, time.Second, 1, 1.0, true},
		{1.6, time.Second, 2, 2.0, true},
		{2.5, time.Second, 3, 3.0, true}, // math.Round(2.5)=3（.5 远离零）
		{1e9, time.Second, 1e9, 1e9, true},
		{1e16, time.Second, math.MaxInt32, math.MaxInt32, true}, // 钳制，防溢出退化
		{0, 0, 0, 0, false},
		{-0.5, 0, 0, 0, false},
	}
	for _, c := range cases {
		period, per, local, ok := quantizeQPS(c.qps)
		if ok != c.ok {
			t.Fatalf("qps=%v ok 期望 %v 实际 %v", c.qps, c.ok, ok)
		}
		if !ok {
			continue
		}
		if per != c.perPeriod || period != c.period {
			t.Fatalf("qps=%v 分布式量化期望 %d/%v，实际 %d/%v", c.qps, c.perPeriod, c.period, per, period)
		}
		if math.Abs(float64(local)-c.local) > 1e-9 {
			t.Fatalf("qps=%v 本地速率期望 %v，实际 %v", c.qps, c.local, local)
		}
	}
}

// TestRateLimiter_小数QPS分布式与fallback一致_L5 qps=0.4/0.6（0<qps<1）时
// 的端到端一致：分布式 GCRA（每 1/qps 秒放行 1 个）与 Redis 故障后的本地
// 降级桶放行模式一致——都是「burst 个内放行、burst 耗尽后拒绝」；
// 本地桶速率与分布式量化一致（修复前 qps=0.4 若被 int(qps+0.5)=0 截断，
// 限速器会退化，此处就是回归护栏）。
func TestRateLimiter_小数QPS分布式与fallback一致_L5(t *testing.T) {
	for _, tc := range []struct {
		qps   float64
		burst int
	}{
		{0.4, 2},
		{0.6, 2},
	} {
		t.Run(fmt.Sprintf("qps=%v", tc.qps), func(t *testing.T) {
			ctx := context.Background()
			mr := miniredis.RunT(t)
			m := newManager(t, mr, "gw-1")

			// 分布式路径（Redis 正常）：GCRA burst 内连续放行，耗尽后拒绝。
			dist := m.NewRateLimiter("m1", tc.qps, tc.burst)
			for i := 0; i < tc.burst; i++ {
				if !dist.Allow(ctx) {
					t.Fatalf("分布式：burst 内第 %d 次应放行", i+1)
				}
			}
			if dist.Allow(ctx) {
				t.Fatal("分布式：burst 耗尽后应拒绝")
			}

			// fallback 路径（Redis 故障 fail-open）：同一量化速率，放行模式一致。
			mr.Close()
			local := m.NewRateLimiter("m1", tc.qps, tc.burst)
			for i := 0; i < tc.burst; i++ {
				if !local.Allow(ctx) {
					t.Fatalf("fallback：burst 内第 %d 次应放行", i+1)
				}
			}
			if local.Allow(ctx) {
				t.Fatal("fallback：burst 耗尽后应拒绝")
			}
		})
	}
}
