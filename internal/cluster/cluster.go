// Package cluster 实现多实例水平部署的协调层，以 Redis 为唯一协调存储：
//
//   - 实例注册：心跳键 {prefix}:instance:{id}（TTL 自动过期），供成员视图查询；
//   - leader 选举：{prefix}:leader 上的 NX+TTL 租约，告警与扩缩容仅 leader
//     执行，避免多实例重复通知/重复建议；
//   - 分布式限流：GCRA 算法，全集群共享同一配额（见 ratelimit.go）；
//   - 会话粘性共享：绑定表落 Redis，跨实例一致（见 session.go）；
//   - 策略广播：admin 热更新经 pub/sub 同步到全部实例（见 policy.go）；
//   - 广播丢失收敛（M10）：每实例把观察到的策略/后端广播写入 Redis 状态
//     快照，leader 周期合并快照并重播——store 未启用时错过的广播也能在
//     有界时间内收敛（见 runStateMirror / runResyncLoop）。
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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"ai-gateway/internal/config"
)

// M10 广播丢失收敛机制的周期与窗口（详见 runStateMirror/runResyncLoop 注释）：
//   - resyncEveryHeartbeats：重播周期 = 心跳间隔 × N；
//   - resyncWindowPeriods：后端条目（含删除墓碑）只重播"内容最后变化时刻
//     在此窗口内"的，窗口外稳定条目永不被重发（避免无谓的实例换代）；
//   - snapshotTTLFactor：状态快照 TTL = 心跳 TTL × N，由重播循环周期性刷新。
const (
	resyncEveryHeartbeats = 4
	resyncWindowPeriods   = 8
	snapshotTTLFactor     = 4
)

// mirrorPolicy 状态镜像中的一条策略；Ver 为内容最后变化的时刻（unix 纳秒），
// 跨实例快照合并时按 Ver 决胜（last-writer-wins）。
type mirrorPolicy struct {
	Origin string `json:"origin"`
	Filter string `json:"filter"`
	Score  string `json:"score"`
	Ver    int64  `json:"ver"`
}

// mirrorBackend 状态镜像中的一个后端条目；Upsert=false 表示删除墓碑。
type mirrorBackend struct {
	Origin string               `json:"origin"`
	Upsert bool                 `json:"upsert"`
	BC     config.BackendConfig `json:"bc,omitempty"`
	Models []string             `json:"models,omitempty"`
	Ver    int64                `json:"ver"`
}

// mirrorState 本实例观察到的集群动态状态（策略 + 动态后端）。
type mirrorState struct {
	Policies map[string]mirrorPolicy  `json:"policies"`
	Backends map[string]mirrorBackend `json:"backends"`
}

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

	// ---- M10：未启用 store 时广播丢失的收敛（见 runStateMirror/runResyncLoop）----
	stateOnce sync.Once   // 镜像与重播循环只启动一次
	mirrorMu  sync.Mutex  // 保护 mirror
	mirror    mirrorState // 本实例观察到的集群状态镜像（含删除墓碑）
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
	return &Manager{
		cfg: cfg, rdb: rdb, keyPrefix: keyPrefix, id: id, addr: addr, errHook: errHook,
		mirror: mirrorState{
			Policies: map[string]mirrorPolicy{},
			Backends: map[string]mirrorBackend{},
		},
	}, nil
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
// 让其余实例立即接任而无需等租约过期。同时启动 M10 收敛所需的
// 状态镜像与重播循环（两者只随首次 Run 启动，见 runStateMirror/runResyncLoop）。
func (m *Manager) Run(ctx context.Context) {
	m.stateOnce.Do(func() {
		go m.runStateMirror(ctx)
		go m.runResyncLoop(ctx)
	})
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

// ===== M10：未启用 store 时 pub/sub 广播丢失的有界时间收敛 =====
//
// 背景：策略/后端广播是 Redis pub/sub fire-and-forget。订阅方在发布后才订阅
// （启动异步 Subscribe）或 pub/sub 断连窗口内，消息永久丢失且不补发；store
// 启用时对账器（internal/reconcile）可以 DB 为事实来源补偿，未启用时没有任何
// 机制——实例与集群分叉直到重启。
//
// 机制（不依赖 store）：
//   - 每实例 runStateMirror 订阅广播频道，把观察到的每条消息并入本地镜像
//     （不跳过自身 origin：发布者也要记录自己的变更），并写入 Redis 状态快照
//     {prefix}:state:{instance}（带 TTL，重播循环周期刷新）；
//   - leader 每"心跳间隔 × resyncEveryHeartbeats"合并全部实例快照（逐条目取
//     Ver 最大者，last-writer-wins）并重播：策略全量重发（幂等、无副作用），
//     后端只重发"内容最后变化时刻在重播窗口内"的条目（含删除墓碑）。
//
// 收敛上界：在线订阅者 ≈ 一个重播周期；错过原广播但仍在窗口内（重播周期 ×
// resyncWindowPeriods）加入的订阅者同样收敛。重播消息保留原发布者 origin，
// 使所有实例（含 leader 自身）的订阅者都能按既有逻辑应用——若重播改用
// leader 自身 origin，订阅方按 origin==self 跳过，leader 错过原广播时将永不收敛。
//
// 取舍（无 store 的固有权衡，非零丢失，有界收敛）：
//   - 后端条目重播会让接收方对该后端重新 Upsert（实例换代，cordon/健康/熔断
//     等实例内状态重置），因此只重发窗口内（近期变更）条目且窗口有界，稳定
//     后端永不被重发（无池抖动）；这与"每次真实变更广播一次换代"的既有语义
//     同量级。
//   - 删除墓碑在窗口内会向已删除实例重复下发 delete（RemoveBackend 报错记
//     日志），窗口结束即止，属有界噪声。
//   - 冷启动瞬间（所有实例的镜像订阅都未就绪时）发布的变更仍可能全部丢失
//     ——该场景只能靠 store 的持久化事实来源覆盖，属文档化边界。

// runStateMirror 状态镜像循环：订阅策略与后端广播频道，把观察到的每条消息
// 并入本地镜像并写 Redis 状态快照，供 leader 周期合并重播。与 main.go 装配的
// RunPolicySubscriber/RunBackendSubscriber 并存（Redis pub/sub 支持同频道
// 多订阅者）；启动时先读本实例既有快照（实例重启后恢复镜像，避免重播断档）。
func (m *Manager) runStateMirror(ctx context.Context) {
	m.bootstrapMirror(ctx)
	sub := m.rdb.Subscribe(ctx, m.key("policy"), m.key("backends"))
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			switch msg.Channel {
			case m.key("policy"):
				m.observePolicy(ctx, msg.Payload)
			case m.key("backends"):
				m.observeBackend(ctx, msg.Payload)
			}
		}
	}
}

// bootstrapMirror 从本实例既有状态快照恢复镜像（首次启动/读失败则从空开始）。
func (m *Manager) bootstrapMirror(ctx context.Context) {
	raw, err := m.rdb.Get(ctx, m.key("state", m.id)).Bytes()
	if err != nil {
		return
	}
	var st mirrorState
	if json.Unmarshal(raw, &st) != nil {
		return
	}
	m.mirrorMu.Lock()
	if st.Policies != nil {
		m.mirror.Policies = st.Policies
	}
	if st.Backends != nil {
		m.mirror.Backends = st.Backends
	}
	m.mirrorMu.Unlock()
}

// observePolicy 把一条策略广播并入镜像。内容未变化（如重播消息）不推进 Ver
// ——Ver 表示"内容最后变化的时刻"，重播不刷新它，保证重播窗口自然衰减、
// 不产生重播-镜像互相刷新的死循环。
func (m *Manager) observePolicy(ctx context.Context, payload string) {
	var p policyMsg
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		slog.Warn("状态镜像解析策略广播失败", "err", err)
		return
	}
	now := time.Now().UnixNano()
	m.mirrorMu.Lock()
	cur := m.mirror.Policies[p.Name]
	if cur.Filter != p.Filter || cur.Score != p.Score {
		m.mirror.Policies[p.Name] = mirrorPolicy{Origin: p.Origin, Filter: p.Filter, Score: p.Score, Ver: now}
	}
	m.mirrorMu.Unlock()
	m.writeSnapshot(ctx)
}

// observeBackend 把一条后端广播并入镜像（delete 记录为墓碑），内容未变化不
// 推进 Ver，同 observePolicy。
func (m *Manager) observeBackend(ctx context.Context, payload string) {
	var p backendMsg
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		slog.Warn("状态镜像解析后端广播失败", "err", err)
		return
	}
	now := time.Now().UnixNano()
	m.mirrorMu.Lock()
	cur := m.mirror.Backends[p.Backend.ID]
	switch {
	case p.Action == BackendDelete && (cur.Upsert || cur.Ver == 0):
		// 删除变为墓碑：此前是存活条目或从未见过；已存在的墓碑不推进 Ver。
		m.mirror.Backends[p.Backend.ID] = mirrorBackend{Origin: p.Origin, Upsert: false, Ver: now}
	case p.Action != BackendDelete && (!cur.Upsert || !reflect.DeepEqual(cur.BC, p.Backend) || !reflect.DeepEqual(cur.Models, p.Models)):
		m.mirror.Backends[p.Backend.ID] = mirrorBackend{Origin: p.Origin, Upsert: true, BC: p.Backend, Models: p.Models, Ver: now}
	}
	m.mirrorMu.Unlock()
	m.writeSnapshot(ctx)
}

// writeSnapshot 把当前镜像写入本实例状态快照键（TTL = 心跳 TTL ×
// snapshotTTLFactor，由 runResyncLoop 周期刷新；写失败只告警——下一轮重试，
// 且镜像仍可从其他实例快照合并恢复）。
func (m *Manager) writeSnapshot(ctx context.Context) {
	m.mirrorMu.Lock()
	raw, err := json.Marshal(m.mirror)
	m.mirrorMu.Unlock()
	if err != nil {
		slog.Warn("序列化状态快照失败", "err", err)
		return
	}
	ttl := m.cfg.HeartbeatTTL.D() * snapshotTTLFactor
	if ttl <= 0 {
		ttl = time.Minute
	}
	if err := m.rdb.Set(ctx, m.key("state", m.id), raw, ttl).Err(); err != nil {
		m.errHook("state_snapshot")
		slog.Warn("写入状态快照失败", "err", err)
	}
}

// runResyncLoop 周期收敛循环：每个周期先刷新本实例快照 TTL（所有实例都做），
// leader 额外执行合并重播（见 replayOnce）。周期 = 心跳间隔 × resyncEveryHeartbeats。
func (m *Manager) runResyncLoop(ctx context.Context) {
	period := m.cfg.HeartbeatInterval.D() * resyncEveryHeartbeats
	if period <= 0 {
		period = time.Second
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.writeSnapshot(ctx)
			if !m.IsLeader() {
				continue
			}
			m.replayOnce(ctx)
		}
	}
}

// replayOnce 合并全部实例快照并重播（仅 leader 执行）。
func (m *Manager) replayOnce(ctx context.Context) {
	m.mirrorMu.Lock()
	merged := mirrorState{
		Policies: make(map[string]mirrorPolicy, len(m.mirror.Policies)),
		Backends: make(map[string]mirrorBackend, len(m.mirror.Backends)),
	}
	for k, v := range m.mirror.Policies {
		merged.Policies[k] = v
	}
	for k, v := range m.mirror.Backends {
		merged.Backends[k] = v
	}
	m.mirrorMu.Unlock()
	for _, st := range m.readSnapshots(ctx) {
		mergeState(&merged, st)
	}

	// 策略：全量重发（幂等，订阅方重复 Set 无副作用），保证任意迟到订阅者
	// 都能在下一个重播周期收敛到策略漂移。
	for name, p := range merged.Policies {
		m.publishResyncPolicy(ctx, name, p.Filter, p.Score, p.Origin)
	}

	// 后端：仅重发"内容最后变化时刻在窗口内"的条目（含删除墓碑）。
	window := m.cfg.HeartbeatInterval.D() * resyncEveryHeartbeats * resyncWindowPeriods
	now := time.Now().UnixNano()
	for id, b := range merged.Backends {
		if time.Duration(now-b.Ver) > window {
			continue
		}
		if b.Upsert {
			m.publishResyncBackend(ctx, BackendUpsert, b.BC, b.Models, b.Origin)
		} else {
			m.publishResyncBackend(ctx, BackendDelete, config.BackendConfig{ID: id}, nil, b.Origin)
		}
	}
}

// readSnapshots 读取全部实例的状态快照（SCAN {prefix}:state:*）。
func (m *Manager) readSnapshots(ctx context.Context) []mirrorState {
	var out []mirrorState
	iter := m.rdb.Scan(ctx, 0, m.key("state", "*"), 100).Iterator()
	for iter.Next(ctx) {
		raw, err := m.rdb.Get(ctx, iter.Val()).Bytes()
		if err != nil {
			continue // 键恰好过期，跳过
		}
		var st mirrorState
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		out = append(out, st)
	}
	return out
}

// mergeState 把 src 合并进 dst：逐条目取 Ver（内容最后变化时刻）更大的版本；
// 同条目 upsert 与删除墓碑也按 Ver 决胜（last-writer-wins）。
func mergeState(dst *mirrorState, src mirrorState) {
	for name, p := range src.Policies {
		if cur, ok := dst.Policies[name]; !ok || p.Ver > cur.Ver {
			dst.Policies[name] = p
		}
	}
	for id, b := range src.Backends {
		if cur, ok := dst.Backends[id]; !ok || b.Ver > cur.Ver {
			dst.Backends[id] = b
		}
	}
}

// publishResyncPolicy 重播一条策略广播：保留原发布者 origin，使所有实例
// （含 leader 自身）的订阅者都能应用。
func (m *Manager) publishResyncPolicy(ctx context.Context, name, filter, score, origin string) {
	raw, _ := json.Marshal(policyMsg{Origin: origin, Name: name, Filter: filter, Score: score})
	if err := m.rdb.Publish(ctx, m.key("policy"), raw).Err(); err != nil {
		m.errHook("policy_resync")
		slog.Warn("策略重播发布失败", "policy", name, "err", err)
	}
}

// publishResyncBackend 重播一条后端变更广播：保留原发布者 origin。
func (m *Manager) publishResyncBackend(ctx context.Context, action string, bc config.BackendConfig, models []string, origin string) {
	raw, _ := json.Marshal(backendMsg{Origin: origin, Action: action, Backend: bc, Models: models})
	if err := m.rdb.Publish(ctx, m.key("backends"), raw).Err(); err != nil {
		m.errHook("backend_resync")
		slog.Warn("后端重播发布失败", "action", action, "backend", bc.ID, "err", err)
	}
}
