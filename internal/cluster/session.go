// 共享会话粘性：绑定表落 Redis（滑动 TTL），全部实例看到一致的
// 会话 -> 后端 绑定，会话不再随负载均衡漂移到不同网关实例而换后端。
// 本地绑定表同步留一份热副本，Redis 故障时无缝退回本地行为。
package cluster

import (
	"context"

	"github.com/redis/go-redis/v9"

	"ai-gateway/internal/session"
)

// SessionStore 共享会话粘性存储，实现 proxy.SessionStore。
type SessionStore struct {
	m     *Manager
	local *session.Store
}

// NewSessionStore 构造共享会话存储；local 为降级用的本地绑定表（必填）。
// TTL 复用 session 配置：Redis 侧用 session.Store 的同一 TTL 做滑动过期。
func (m *Manager) NewSessionStore(local *session.Store) *SessionStore {
	return &SessionStore{m: m, local: local}
}

// Lookup 查询绑定：Redis 命中即滑动续期；未命中回查本地热副本
// （覆盖 Redis 数据丢失场景）；Redis 故障直接退回本地。
func (s *SessionStore) Lookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.m.cfg.OpTimeout.D())
	defer cancel()
	id, err := s.m.rdb.GetEx(ctx, s.m.key("session", key), s.local.TTL()).Result()
	switch {
	case err == nil:
		return id, true
	case err == redis.Nil:
		return s.local.Lookup(key)
	default:
		s.m.errHook("session")
		return s.local.Lookup(key)
	}
}

// Bind 写入绑定：Redis 与本地热副本双写；Redis 写失败只计数不阻断。
func (s *SessionStore) Bind(key, backendID string) {
	if key == "" {
		return
	}
	s.local.Bind(key, backendID)
	ctx, cancel := context.WithTimeout(context.Background(), s.m.cfg.OpTimeout.D())
	defer cancel()
	if err := s.m.rdb.Set(ctx, s.m.key("session", key), backendID, s.local.TTL()).Err(); err != nil {
		s.m.errHook("session")
	}
}
