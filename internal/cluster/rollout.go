// 金丝雀发布状态广播（M11）：集群模式下由 leader 单主评估并广播发布步进，
// 跟随者只应用广播（ApplyStep），避免无流量实例卡死不晋级、各实例权重分叉。
//
// 收敛语义：与 M10 状态镜像不同，rollout 广播不要求零丢失——某实例错过一次
// 步进会在下一次广播（下一步晋级/回滚/完成）时收敛到同一状态与权重；最终
// 一致由"每次状态变更都广播"保证。订阅方忽略自身 origin，避免 leader 应用
// 自己的广播重置观察窗口。
package cluster

import (
	"context"
	"encoding/json"
	"log/slog"

	"ai-gateway/internal/rollout"
)

// rolloutMsg 发布步进消息体。
type rolloutMsg struct {
	Origin  string `json:"origin"`
	Model   string `json:"model"`
	State   string `json:"state"`
	StepIdx int    `json:"step_idx"`
}

// PublishRolloutStep 广播一条发布步进（leader 在状态变更后调用）。
func (m *Manager) PublishRolloutStep(ctx context.Context, ev rollout.StepEvent) error {
	raw, _ := json.Marshal(rolloutMsg{Origin: m.id, Model: ev.Model, State: ev.State, StepIdx: ev.StepIdx})
	if err := m.rdb.Publish(ctx, m.key("rollout"), raw).Err(); err != nil {
		m.errHook("rollout_publish")
		return err
	}
	return nil
}

// RunRolloutSubscriber 订阅发布步进并应用到本地发布管理器，随 ctx 退出。
// 忽略自身 origin：leader 不应应用自己广播的步进（会把观察窗口重置到接收时刻，
// 推迟下一阶评估）。
func (m *Manager) RunRolloutSubscriber(ctx context.Context, apply func(ev rollout.StepEvent) error) {
	m.runSubscriber(ctx, "rollout", func(payload string) {
		var msg rolloutMsg
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			slog.Error("rollout 广播解析失败", "err", err)
			return
		}
		if msg.Origin == m.id {
			return
		}
		if err := apply(rollout.StepEvent{Model: msg.Model, State: msg.State, StepIdx: msg.StepIdx}); err != nil {
			slog.Error("应用 rollout 广播失败", "model", msg.Model, "state", msg.State, "err", err)
		}
	})
}
