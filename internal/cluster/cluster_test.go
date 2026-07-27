package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"ai-gateway/internal/config"
	"ai-gateway/internal/session"
)

// testConfig 构造指向 miniredis 的集群配置（短周期，加速测试）。
func testConfig(addr, id string) config.ClusterConfig {
	c := config.ClusterConfig{
		Enabled:           true,
		RedisAddr:         addr,
		InstanceID:        id,
		KeyPrefix:         "testgw",
		HeartbeatInterval: config.Duration(20 * time.Millisecond),
		HeartbeatTTL:      config.Duration(200 * time.Millisecond),
		LeaderTTL:         config.Duration(200 * time.Millisecond),
		RateLimitMode:     "distributed",
		SessionMode:       "shared",
		OpTimeout:         config.Duration(500 * time.Millisecond),
	}
	return c
}

func newManager(t *testing.T, mr *miniredis.Miniredis, id string) *Manager {
	t.Helper()
	m, err := New(testConfig(mr.Addr(), id), ":8080", nil)
	if err != nil {
		t.Fatalf("构造管理器失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// waitFor 轮询等待条件成立，超时判失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestLeaderElection(t *testing.T) {
	mr := miniredis.RunT(t)
	m1 := newManager(t, mr, "gw-1")
	m2 := newManager(t, mr, "gw-2")

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go m1.Run(ctx1)
	waitFor(t, time.Second, m1.IsLeader, "gw-1 未能当选 leader")

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go m2.Run(ctx2)
	time.Sleep(100 * time.Millisecond)
	if m2.IsLeader() {
		t.Fatal("租约被 gw-1 持有期间，gw-2 不应当选")
	}

	// gw-1 退出后主动让出租约，gw-2 应接任。
	cancel1()
	waitFor(t, time.Second, m2.IsLeader, "gw-1 让出后 gw-2 未接任 leader")
}

func TestLeaderExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	m1 := newManager(t, mr, "gw-1")
	m2 := newManager(t, mr, "gw-2")

	// 手工驱动 tick，模拟 gw-1 抢到租约后失联（不再续期、也不主动让出）。
	m1.tick(context.Background())
	if !m1.IsLeader() {
		t.Fatal("gw-1 应当选 leader")
	}
	m2.tick(context.Background())
	if m2.IsLeader() {
		t.Fatal("租约未过期，gw-2 不应当选")
	}

	// miniredis 的 TTL 不随真实时间流逝，用 FastForward 推进到租约过期。
	mr.FastForward(300 * time.Millisecond)
	m2.tick(context.Background())
	if !m2.IsLeader() {
		t.Fatal("租约过期后 gw-2 应接任 leader")
	}
}

func TestMembers(t *testing.T) {
	mr := miniredis.RunT(t)
	m1 := newManager(t, mr, "gw-1")
	m2 := newManager(t, mr, "gw-2")
	m1.tick(context.Background())
	m2.tick(context.Background())

	members, err := m1.Members(context.Background())
	if err != nil {
		t.Fatalf("查询成员失败: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("期望 2 个成员，实际 %d", len(members))
	}
	leaders := 0
	for _, mb := range members {
		if mb.Leader {
			leaders++
			if mb.ID != "gw-1" {
				t.Fatalf("leader 应为 gw-1，实际 %s", mb.ID)
			}
		}
	}
	if leaders != 1 {
		t.Fatalf("期望恰好 1 个 leader，实际 %d", leaders)
	}

	// 心跳过期的实例应从成员视图消失。
	mr.FastForward(300 * time.Millisecond)
	members, err = m1.Members(context.Background())
	if err != nil {
		t.Fatalf("查询成员失败: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("心跳过期后成员应为空，实际 %d", len(members))
	}
}

func TestRateLimiterShared(t *testing.T) {
	mr := miniredis.RunT(t)
	// 两个实例的限流器指向同一 Redis 键，应共享同一份配额。
	l1 := newManager(t, mr, "gw-1").NewRateLimiter("m1", 5, 5)
	l2 := newManager(t, mr, "gw-2").NewRateLimiter("m1", 5, 5)

	ctx := context.Background()
	allowed := 0
	for i := 0; i < 20; i++ {
		// 交替从两个实例放行，验证消耗的是同一份配额。
		var ok bool
		if i%2 == 0 {
			ok = l1.Allow(ctx)
		} else {
			ok = l2.Allow(ctx)
		}
		if ok {
			allowed++
		}
	}
	// GCRA 突发容量为 burst=5，瞬时 20 连发只应放行约 5 个。
	if allowed < 4 || allowed > 6 {
		t.Fatalf("期望放行约 5 个（burst），实际 %d", allowed)
	}
}

func TestRateLimiterFallback(t *testing.T) {
	mr := miniredis.RunT(t)
	errOps := make(chan string, 16)
	m, err := New(testConfig(mr.Addr(), "gw-1"), ":8080", func(op string) { errOps <- op })
	if err != nil {
		t.Fatalf("构造管理器失败: %v", err)
	}
	defer m.Close()
	l := m.NewRateLimiter("m1", 100, 3)

	// Redis 故障后应退回本地令牌桶（fail-open），且上报错误计数。
	mr.Close()
	if !l.Allow(context.Background()) {
		t.Fatal("Redis 故障时应退回本地令牌桶放行")
	}
	select {
	case op := <-errOps:
		if op != "ratelimit" {
			t.Fatalf("期望上报 ratelimit 错误，实际 %s", op)
		}
	default:
		t.Fatal("Redis 故障未上报错误计数")
	}
}

func TestSessionStoreShared(t *testing.T) {
	mr := miniredis.RunT(t)
	local1 := session.NewStore(time.Minute, 100)
	local2 := session.NewStore(time.Minute, 100)
	s1 := newManager(t, mr, "gw-1").NewSessionStore(local1)
	s2 := newManager(t, mr, "gw-2").NewSessionStore(local2)

	// gw-1 上绑定的会话，gw-2 应立即可见（跨实例粘性）。
	s1.Bind("sess-a", "backend-1")
	if id, ok := s2.Lookup("sess-a"); !ok || id != "backend-1" {
		t.Fatalf("跨实例查询绑定失败: id=%q ok=%v", id, ok)
	}

	// TTL 过期后绑定消失（本地热副本也过期才算彻底消失，
	// 这里只推进 Redis 时钟，本地副本由 gw-2 覆盖：它从未 Bind 过）。
	mr.FastForward(2 * time.Minute)
	if _, ok := s2.Lookup("sess-a"); ok {
		t.Fatal("TTL 过期后绑定应消失")
	}
}

func TestSessionStoreFallback(t *testing.T) {
	mr := miniredis.RunT(t)
	local := session.NewStore(time.Minute, 100)
	m, err := New(testConfig(mr.Addr(), "gw-1"), ":8080", nil)
	if err != nil {
		t.Fatalf("构造管理器失败: %v", err)
	}
	defer m.Close()
	s := m.NewSessionStore(local)

	s.Bind("sess-a", "backend-1")
	// Redis 故障后应退回本地热副本，绑定仍可查。
	mr.Close()
	if id, ok := s.Lookup("sess-a"); !ok || id != "backend-1" {
		t.Fatalf("Redis 故障后本地热副本查询失败: id=%q ok=%v", id, ok)
	}
}

func TestPolicyPubSub(t *testing.T) {
	mr := miniredis.RunT(t)
	m1 := newManager(t, mr, "gw-1")
	m2 := newManager(t, mr, "gw-2")

	type update struct{ name, filter, score string }
	got := make(chan update, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// gw-2 订阅；gw-1 自身也订阅，用于验证自身消息被跳过。
	go m2.RunPolicySubscriber(ctx, func(n, f, s string) error {
		got <- update{n, f, s}
		return nil
	})
	selfGot := make(chan update, 4)
	go m1.RunPolicySubscriber(ctx, func(n, f, s string) error {
		selfGot <- update{n, f, s}
		return nil
	})
	time.Sleep(50 * time.Millisecond) // 等订阅建立

	if err := m1.PublishPolicy(ctx, "p1", "healthy", "running"); err != nil {
		t.Fatalf("发布策略失败: %v", err)
	}

	select {
	case u := <-got:
		if u.name != "p1" || u.filter != "healthy" || u.score != "running" {
			t.Fatalf("收到的策略内容不符: %+v", u)
		}
	case <-time.After(time.Second):
		t.Fatal("gw-2 未收到策略广播")
	}
	select {
	case u := <-selfGot:
		t.Fatalf("gw-1 不应应用自身发布的消息: %+v", u)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestLeaderDemotionOnPartition 验证网络分区双主防护：与 Redis 断连后，
// 持续拿不到租约确认超过一个租约期，旧 leader 主动降级。
func TestLeaderDemotionOnPartition(t *testing.T) {
	mr := miniredis.RunT(t)
	m := newManager(t, mr, "i1")

	m.tick(context.Background())
	if !m.IsLeader() {
		t.Fatal("首次 tick 应当选 leader")
	}

	// 模拟分区：Redis 不可达。租约期（200ms）内的失败保持判定，超过后降级。
	mr.Close()
	m.tick(context.Background())
	if !m.IsLeader() {
		t.Fatal("租约期内的瞬时故障不应立即降级")
	}
	time.Sleep(250 * time.Millisecond)
	m.tick(context.Background())
	if m.IsLeader() {
		t.Fatal("租约确认持续失败超过租约期后应主动降级，消除双主窗口")
	}
}

// TestUniversalClientAddrs 验证 redis_addrs 列表形态（UniversalClient 单地址路径）
// 与既有 redis_addr 行为一致：能连接、能选举。
func TestUniversalClientAddrs(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := testConfig("", "u1")
	cfg.RedisAddr = ""
	cfg.RedisAddrs = []string{mr.Addr()}
	m, err := New(cfg, ":8080", nil)
	if err != nil {
		t.Fatalf("redis_addrs 形态构造失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.tick(context.Background())
	if !m.IsLeader() {
		t.Fatal("redis_addrs 形态应正常完成选举")
	}
}
