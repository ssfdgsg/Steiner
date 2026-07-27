// Package alerting 实现指标告警与自动扩缩容建议：
//
//   - 告警规则（expr 布尔表达式）按周期对后端/集群求值，
//     状态机 inactive -> pending(持续 For) -> firing -> resolved，
//     跃迁时向 webhook 目标推送事件；
//   - 自动扩缩容建议器按模型路由求值扩/缩表达式，产出期望副本数建议，
//     经 webhook 推送给外部控制器（K8s operator、运维机器人等）落地，
//     网关自身不执行扩缩容。
//
// 表达式与调度策略共用同一套变量语义（见 BackendEnv / ClusterEnv），
// 运维只需掌握一种表达式方言。
package alerting

import "time"

// 事件类型。
const (
	TypeAlert     = "alert"     // 告警状态跃迁
	TypeAutoscale = "autoscale" // 扩缩容建议
	TypeRollout   = "rollout"   // 金丝雀发布状态跃迁（internal/rollout 产生）
)

// 事件状态。
const (
	StatusFiring         = "firing"
	StatusResolved       = "resolved"
	StatusRecommendation = "recommendation"
)

// Event 为一次推送到 webhook 的事件。generic 模板直接序列化本结构，
// 字段命名保持稳定，供自动扩容控制器等程序化消费方解析。
type Event struct {
	Type     string `json:"type"`               // alert | autoscale
	Status   string `json:"status"`             // firing | resolved | recommendation
	Rule     string `json:"rule,omitempty"`     // 告警规则名
	Severity string `json:"severity,omitempty"` // info | warning | critical
	Scope    string `json:"scope,omitempty"`    // backend | cluster
	// Instance 触发实例：backend 作用域为后端 ID，cluster 作用域为模型路由名。
	Instance string    `json:"instance,omitempty"`
	Expr     string    `json:"expr,omitempty"`
	Summary  string    `json:"summary"`
	StartsAt time.Time `json:"starts_at,omitempty"`
	EndsAt   time.Time `json:"ends_at,omitempty"`
	// Labels 规则自定义标签，供接收方路由/分组。
	Labels map[string]string `json:"labels,omitempty"`
	// Vars 触发时刻的关键数值变量快照（求值环境中的数值项）。
	Vars map[string]float64 `json:"vars,omitempty"`
	// Scale 仅 autoscale 事件携带。
	Scale *ScaleAdvice `json:"scale,omitempty"`
}

// ScaleAdvice 为扩缩容建议明细。
type ScaleAdvice struct {
	Model string `json:"model"`
	// CurrentReplicas 路由内配置的后端实例数。
	CurrentReplicas int `json:"current_replicas"`
	// AvailableReplicas 当前实际可参与调度的实例数。
	AvailableReplicas int `json:"available_replicas"`
	// DesiredReplicas 建议副本数（已按 min/max 钳位）。
	DesiredReplicas int    `json:"desired_replicas"`
	Direction       string `json:"direction"` // up | down
}
