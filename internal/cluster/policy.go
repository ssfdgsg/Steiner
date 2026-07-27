// 策略热更新广播：admin 在任一实例 PUT 策略后，经 Redis pub/sub 同步到
// 全部实例，保证集群内调度逻辑一致。消息携带来源实例 ID，自身消息跳过。
package cluster

import (
	"context"
	"encoding/json"
	"log/slog"
)

// policyMsg 策略广播消息体。
type policyMsg struct {
	Origin string `json:"origin"`
	Name   string `json:"name"`
	Filter string `json:"filter"`
	Score  string `json:"score"`
}

// PublishPolicy 广播一条策略变更（策略已在本实例编译通过后调用）。
func (m *Manager) PublishPolicy(ctx context.Context, name, filter, score string) error {
	raw, _ := json.Marshal(policyMsg{Origin: m.id, Name: name, Filter: filter, Score: score})
	if err := m.rdb.Publish(ctx, m.key("policy"), raw).Err(); err != nil {
		m.errHook("policy_publish")
		return err
	}
	return nil
}

// RunPolicySubscriber 订阅策略广播并应用到本地策略引擎，随 ctx 退出。
// apply 即 policy.Engine.Set：编译失败只记日志（发布方已验证过表达式，
// 失败通常意味着版本不一致，属于需人工介入的异常）。
func (m *Manager) RunPolicySubscriber(ctx context.Context, apply func(name, filter, score string) error) {
	m.runSubscriber(ctx, "policy", func(payload string) {
		var p policyMsg
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			slog.Warn("策略广播消息解析失败", "err", err)
			return
		}
		if p.Origin == m.id {
			return
		}
		if err := apply(p.Name, p.Filter, p.Score); err != nil {
			slog.Error("应用广播策略失败", "policy", p.Name, "origin", p.Origin, "err", err)
			return
		}
		slog.Info("已应用广播策略", "policy", p.Name, "origin", p.Origin)
	})
}

// runSubscriber 订阅骨架：订阅指定频道、循环消费消息、随 ctx 退出。
// 策略与后端广播共用，共性行为（断连退出、消息分发）只维护一处。
func (m *Manager) runSubscriber(ctx context.Context, channel string, handle func(payload string)) {
	sub := m.rdb.Subscribe(ctx, m.key(channel))
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
			handle(msg.Payload)
		}
	}
}
