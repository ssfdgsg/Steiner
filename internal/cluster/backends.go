// 动态后端变更广播：admin 在任一实例增删后端后，经 Redis pub/sub 同步到
// 全部实例，保证集群内后端池一致。持久化由发起实例完成，订阅方只改内存态。
package cluster

import (
	"context"
	"encoding/json"
	"log/slog"

	"ai-gateway/internal/config"
)

// 动态后端变更动作。
const (
	BackendUpsert = "upsert"
	BackendDelete = "delete"
)

// backendMsg 后端变更广播消息体。
type backendMsg struct {
	Origin  string               `json:"origin"`
	Action  string               `json:"action"` // upsert | delete
	Backend config.BackendConfig `json:"backend"`
	Models  []string             `json:"models,omitempty"`
}

// PublishBackendChange 广播一条后端变更（变更已在本实例生效后调用）。
func (m *Manager) PublishBackendChange(ctx context.Context, action string, bc config.BackendConfig, models []string) error {
	raw, _ := json.Marshal(backendMsg{Origin: m.id, Action: action, Backend: bc, Models: models})
	if err := m.rdb.Publish(ctx, m.key("backends"), raw).Err(); err != nil {
		m.errHook("backend_publish")
		return err
	}
	return nil
}

// RunBackendSubscriber 订阅后端变更并应用到本地注册表，随 ctx 退出。
func (m *Manager) RunBackendSubscriber(ctx context.Context, apply func(action string, bc config.BackendConfig, models []string) error) {
	m.runSubscriber(ctx, "backends", func(payload string) {
		var p backendMsg
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			slog.Warn("后端变更广播消息解析失败", "err", err)
			return
		}
		if p.Origin == m.id {
			return
		}
		if err := apply(p.Action, p.Backend, p.Models); err != nil {
			slog.Error("应用广播后端变更失败", "action", p.Action, "backend", p.Backend.ID, "origin", p.Origin, "err", err)
			return
		}
		slog.Info("已应用广播后端变更", "action", p.Action, "backend", p.Backend.ID, "origin", p.Origin)
	})
}
