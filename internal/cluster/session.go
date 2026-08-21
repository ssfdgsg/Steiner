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

// Unbind 解除单个会话的绑定：Redis 共享键与本地热副本同步删除。
// 供失效绑定清理路径使用（如 Lookup 命中指向已摘除/不可用后端的绑定）；
// Redis 故障时降级为仅清本地并上报错误，不阻断调用方。
func (s *SessionStore) Unbind(key string) {
	if key == "" {
		return
	}
	s.local.Unbind(key)
	ctx, cancel := context.WithTimeout(context.Background(), s.m.cfg.OpTimeout.D())
	defer cancel()
	if err := s.m.rdb.Del(ctx, s.m.key("session", key)).Err(); err != nil {
		s.m.errHook("session")
	}
}

// InvalidateBackend 后端下线/摘除时跨集群批量清除其全部会话绑定：
// 本地热副本 + 扫描 Redis 共享表删除所有指向该后端的绑定键。
// 健康翻转回调与后端摘除钩子调用之（M14：不再只清本地表）。
// Redis 故障时降级为仅本地生效并上报错误，本地行为始终正确。
func (s *SessionStore) InvalidateBackend(backendID string) {
	s.local.InvalidateBackend(backendID)
	ctx, cancel := context.WithTimeout(context.Background(), s.m.cfg.OpTimeout.D())
	defer cancel()
	if err := s.deleteSharedByBackend(ctx, backendID); err != nil {
		s.m.errHook("session")
	}
}

// deleteSharedByBackend 用 SCAN 枚举共享绑定键，批量取出值与 backendID 比对，
// 只删除匹配者（避免 DELETE 误伤期间被改绑到其他后端的键）。跨实例并发
// 改绑与清除存在天然竞态，但清除是自愈式的：残留绑定会被下一次 Lookup
// 命中后在代理层解除，或由下一次摘除/健康翻转再次清理。
func (s *SessionStore) deleteSharedByBackend(ctx context.Context, backendID string) error {
	match := s.m.key("session", "*")
	var cursor uint64
	for {
		keys, next, err := s.m.rdb.Scan(ctx, cursor, match, 64).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			vals, err := s.m.rdb.MGet(ctx, keys...).Result()
			if err != nil {
				return err
			}
			var toDelete []string
			for i, v := range vals {
				if id, ok := v.(string); ok && id == backendID {
					toDelete = append(toDelete, keys[i])
				}
			}
			if len(toDelete) > 0 {
				if err := s.m.rdb.Del(ctx, toDelete...).Err(); err != nil {
					return err
				}
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}
