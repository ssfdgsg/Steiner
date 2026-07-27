// Package cluster 实现多实例水平部署的协调层，以 Redis 为唯一协调存储：
//
//   - 实例注册：心跳键 {prefix}:instance:{id}（TTL 自动过期），供成员视图查询；
//   - leader 选举：{prefix}:leader 上的 NX+TTL 租约，告警与扩缩容仅 leader
//     执行，避免多实例重复通知/重复建议；
//   - 分布式限流：GCRA 算法，全集群共享同一配额（见 ratelimit.go）；
//   - 会话粘性共享：绑定表落 Redis，跨实例一致（见 session.go）；
//   - 策略广播：admin 热更新经 pub/sub 同步到全部实例（见 policy.go）。
//
// 降级原则：Redis 故障不影响数据面——限流退回本地令牌桶、会话退回本地
// 绑定表；仅协调能力（选举/广播/成员视图）暂停，Redis 恢复后自动收敛。
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"ai-gateway/internal/config"
)

// renewLeaderScript leader 租约的获取与续期：
// 键不存在则抢占；持有者是自己则续期；否则失败。原子执行避免误抢他人租约。
var renewLeaderScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur == false then
	redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
	return 1
elseif cur == ARGV[1] then
	redis.call('PEXPIRE', KEYS[1], ARGV[2])
	return 1
else
	return 0
end
`)

// Member 成员视图中的一个网关实例。
type Member struct {
	ID       string    `json:"id"`
	Addr     string    `json:"addr"`
	Leader   bool      `json:"leader"`
	LastSeen time.Time `json:"last_seen"`
}

// heartbeatValue 心跳键的存储值。
type heartbeatValue struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	LastSeen int64  `json:"last_seen"` // unix 秒
}

// Manager 集群协调管理器。除 Run/RunPolicySubscriber 外的方法均并发安全。
type Manager struct {
	cfg       config.ClusterConfig
	rdb       redis.UniversalClient
	keyPrefix string
	id        string
	addr      string

	leader   atomic.Bool
	onLeader func(bool)   // leader 状态变更回调（刷指标）
	errHook  func(string) // Redis 操作失败回调（按操作名计数）

	// lastRenew 最近一次租约操作得到 Redis 确认的时刻（仅 Run/tick 单协程访问）。
	lastRenew time.Time
}

// New 构造管理器并验证 Redis 连通性；连不上即返回错误（启动期左移暴露）。
// addr 为本实例对外地址（监听地址），errHook 在每次 Redis 操作失败时回调。
//
// 部署形态由配置推导（UniversalClient 语义）：
//   - redis_master_name 非空 → Sentinel 故障转移客户端（HA 推荐形态）；
//   - redis_addrs 多地址 → Redis Cluster 客户端；
//   - 其余 → 单机客户端。
func New(cfg config.ClusterConfig, addr string, errHook func(op string)) (*Manager, error) {
	if errHook == nil {
		errHook = func(string) {}
	}
	id := cfg.InstanceID
	if id == "" {
		host, _ := os.Hostname()
		id = host + strings.ReplaceAll(addr, ":", "-")
	}

	addrs := cfg.RedisAddrs
	if len(addrs) == 0 {
		addrs = []string{cfg.RedisAddr}
	}
	keyPrefix := cfg.KeyPrefix
	if cfg.RedisMasterName == "" && len(addrs) > 1 {
		// Redis Cluster 模式：键前缀加 hash tag，使全部协调键落在同一 slot——
		// 协调数据量极小无分片价值，同 slot 保证 SCAN 成员视图完整、脚本单键执行。
		keyPrefix = "{" + keyPrefix + "}"
	}
	rdb := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:        addrs,
		MasterName:   cfg.RedisMasterName,
		Username:     cfg.RedisUsername,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  cfg.OpTimeout.D(),
		WriteTimeout: cfg.OpTimeout.D(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("连接集群协调 Redis %v 失败: %w", addrs, err)
	}
	return &Manager{cfg: cfg, rdb: rdb, keyPrefix: keyPrefix, id: id, addr: addr, errHook: errHook}, nil
}

// ID 返回本实例标识。
func (m *Manager) ID() string { return m.id }

// IsLeader 返回本实例当前是否持有 leader 租约。
func (m *Manager) IsLeader() bool { return m.leader.Load() }

// OnLeaderChange 注册 leader 状态变更回调（装配期调用，Run 之前）。
func (m *Manager) OnLeaderChange(fn func(leader bool)) { m.onLeader = fn }

// Close 释放 Redis 连接。
func (m *Manager) Close() error { return m.rdb.Close() }

// key 拼接带前缀的键名（cluster 模式下前缀含 hash tag，见 New）。
func (m *Manager) key(parts ...string) string {
	return m.keyPrefix + ":" + strings.Join(parts, ":")
}

// Run 心跳与选举主循环，随 ctx 退出；退出时若持有租约则主动让出，
// 让其余实例立即接任而无需等租约过期。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.HeartbeatInterval.D())
	defer ticker.Stop()
	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			m.resign()
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

// tick 一次心跳 + 一次租约获取/续期。
func (m *Manager) tick(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, m.cfg.OpTimeout.D())
	defer cancel()

	val, _ := json.Marshal(heartbeatValue{ID: m.id, Addr: m.addr, LastSeen: time.Now().Unix()})
	if err := m.rdb.Set(opCtx, m.key("instance", m.id), val, m.cfg.HeartbeatTTL.D()).Err(); err != nil {
		m.errHook("heartbeat")
		slog.Warn("集群心跳写入失败", "err", err)
	}

	got, err := renewLeaderScript.Run(opCtx, m.rdb,
		[]string{m.key("leader")}, m.id, m.cfg.LeaderTTL.D().Milliseconds()).Int()
	if err != nil {
		m.errHook("leader")
		slog.Warn("集群 leader 租约操作失败", "err", err)
		// 短暂故障保持上一次判定（Redis 全挂时他人同样抢不走租约）；
		// 但持续拿不到确认超过一个租约期后必须主动降级：网络分区场景下
		// （仅本实例与 Redis 断连）租约此刻已可被其他实例抢占，继续自认
		// leader 会与新 leader 并存，造成告警/扩缩容双主重复执行。
		// 降级时刻与租约过期时刻对齐，双主窗口收敛到时钟漂移量级。
		if m.leader.Load() && !m.lastRenew.IsZero() && time.Since(m.lastRenew) >= m.cfg.LeaderTTL.D() {
			slog.Warn("leader 租约确认持续失败超过租约期，主动降级", "instance", m.id)
			m.setLeader(false)
		}
		return
	}
	m.lastRenew = time.Now()
	m.setLeader(got == 1)
}

// resign 主动让出租约（仅当持有者是自己时删除）。
func (m *Manager) resign() {
	if !m.leader.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.OpTimeout.D())
	defer cancel()
	// 用脚本校验持有者，避免误删他人刚抢到的租约。
	_ = m.rdb.Eval(ctx, `
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`, []string{m.key("leader")}, m.id).Err()
	m.setLeader(false)
}

// setLeader 更新本地 leader 状态，变更时记日志并触发回调。
func (m *Manager) setLeader(v bool) {
	if m.leader.Swap(v) == v {
		return
	}
	if v {
		slog.Info("本实例当选集群 leader", "instance", m.id)
	} else {
		slog.Info("本实例不再是集群 leader", "instance", m.id)
	}
	if m.onLeader != nil {
		m.onLeader(v)
	}
}

// Members 返回当前存活的实例列表（按心跳键扫描），并标注 leader。
func (m *Manager) Members(ctx context.Context) ([]Member, error) {
	leaderID, err := m.rdb.Get(ctx, m.key("leader")).Result()
	if err != nil && err != redis.Nil {
		m.errHook("members")
		return nil, err
	}

	var members []Member
	iter := m.rdb.Scan(ctx, 0, m.key("instance", "*"), 100).Iterator()
	for iter.Next(ctx) {
		raw, err := m.rdb.Get(ctx, iter.Val()).Result()
		if err != nil {
			continue // 心跳键恰好过期，跳过
		}
		var hb heartbeatValue
		if json.Unmarshal([]byte(raw), &hb) != nil {
			continue
		}
		members = append(members, Member{
			ID:       hb.ID,
			Addr:     hb.Addr,
			Leader:   hb.ID == leaderID,
			LastSeen: time.Unix(hb.LastSeen, 0),
		})
	}
	if err := iter.Err(); err != nil {
		m.errHook("members")
		return nil, err
	}
	return members, nil
}
