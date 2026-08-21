// Package session 实现会话粘性存储：会话键 -> 后端 ID 的绑定表。
// 同一会话的多轮请求固定路由到同一后端，与 kvcache 前缀亲和互补：
// 粘性是确定性 O(1) 查表，前缀匹配是概率性收益，多轮对话两者叠加
// 可获得最高的后端 KV cache 命中率。
//
// 实现：分片 map（降低锁竞争）+ 滑动 TTL + 容量上限（超限淘汰分片内最久未用项）。
// 单机内存态；多副本部署时需由前置 LB 按会话键做一致性哈希（见 README）。
package session

import (
	"hash/fnv"
	"sync"
	"time"
)

const shardCount = 64

type entry struct {
	backendID string
	expiresAt int64 // unix 纳秒，滑动续期
}

type shard struct {
	mu sync.Mutex
	m  map[string]entry
}

// Store 会话粘性存储。
type Store struct {
	shards     [shardCount]*shard
	ttl        time.Duration
	maxPerShrd int
}

// NewStore 构造存储。maxEntries 为全局容量上限（内部均摊到各分片）。
func NewStore(ttl time.Duration, maxEntries int) *Store {
	s := &Store{ttl: ttl, maxPerShrd: maxEntries / shardCount}
	if s.maxPerShrd < 1 {
		s.maxPerShrd = 1
	}
	for i := range s.shards {
		s.shards[i] = &shard{m: map[string]entry{}}
	}
	return s
}

// TTL 返回绑定的滑动过期时长（集群共享存储复用同一 TTL）。
func (s *Store) TTL() time.Duration { return s.ttl }

func (s *Store) shardOf(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return s.shards[h.Sum32()%shardCount]
}

// Lookup 查询绑定；命中时滑动续期。
func (s *Store) Lookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	sh := s.shardOf(key)
	now := time.Now().UnixNano()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e, ok := sh.m[key]
	if !ok || e.expiresAt < now {
		if ok {
			delete(sh.m, key)
		}
		return "", false
	}
	e.expiresAt = now + s.ttl.Nanoseconds()
	sh.m[key] = e
	return e.backendID, true
}

// Bind 建立（或改绑）会话到后端的粘性；分片超容时淘汰最久未用项。
func (s *Store) Bind(key, backendID string) {
	if key == "" || backendID == "" {
		return
	}
	sh := s.shardOf(key)
	now := time.Now().UnixNano()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, exists := sh.m[key]; !exists && len(sh.m) >= s.maxPerShrd {
		evictOldest(sh.m)
	}
	sh.m[key] = entry{backendID: backendID, expiresAt: now + s.ttl.Nanoseconds()}
}

// Unbind 移除指定会话的绑定（幂等：不存在即无操作）。
// 供失效绑定清理路径使用：Lookup 命中指向已摘除/不可用后端的绑定、
// 或主动解绑某会话时调用。共享存储（cluster.SessionStore）同时删除
// Redis 侧键。
func (s *Store) Unbind(key string) {
	if key == "" {
		return
	}
	sh := s.shardOf(key)
	sh.mu.Lock()
	delete(sh.m, key)
	sh.mu.Unlock()
}

// InvalidateBackend 后端下线/摘除时批量解绑其全部会话。
func (s *Store) InvalidateBackend(backendID string) {
	for _, sh := range s.shards {
		sh.mu.Lock()
		for k, e := range sh.m {
			if e.backendID == backendID {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
}

// Len 当前绑定数（跨分片求和，仅用于观测）。
func (s *Store) Len() int {
	n := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// evictOldest 淘汰分片内过期或最早到期的一项（近似 LRU：滑动续期使
// 活跃会话的到期时间靠后，最早到期即最久未活跃）。
func evictOldest(m map[string]entry) {
	var oldestKey string
	var oldest int64
	for k, e := range m {
		if oldestKey == "" || e.expiresAt < oldest {
			oldestKey, oldest = k, e.expiresAt
		}
	}
	if oldestKey != "" {
		delete(m, oldestKey)
	}
}
