// 表达式求值环境构建。变量语义与调度策略表达式保持一致，
// 运维在告警/扩缩容规则中使用与调度算式同一套变量名。
package alerting

import (
	"sort"
	"time"

	"ai-gateway/internal/backend"
)

// BackendEnv 构建 backend 作用域的求值环境（逐后端）。
func BackendEnv(b *backend.Backend, now time.Time) map[string]interface{} {
	snap := b.Snapshot()
	return map[string]interface{}{
		"backend":       b.ID,
		"engine":        string(b.Engine),
		"engine_family": b.Engine.Family(),
		"weight":        b.Weight,
		"healthy":       b.Healthy(),
		"available":     b.Available(now),
		"cordoned":      b.Cordoned(),
		"ejected":       b.Ejected(now),
		"scrape_err":    snap.Err != "",
		"inflight":      float64(b.Inflight()),
		"running":       snap.Running,
		"waiting":       snap.Waiting,
		"kv_usage":      snap.KVUsage,
		"hit_rate":      snap.HitRate,
		"gen_tps":       snap.GenTokPerSec,
		"labels":        b.Labels,
		"raw":           snap.Raw,     // 后端 /metrics 原始指标（含 rate: 派生值）
		"vars":          b.PromVars(), // 外部 Prometheus 注入变量
	}
}

// ClusterEnv 构建 cluster 作用域的求值环境（逐模型路由聚合）。
// 负载类聚合只统计当前可参与调度的实例；无可用实例时聚合值为 0，
// 可用 available_count == 0 单独告警。
func ClusterEnv(model string, pool []*backend.Backend, now time.Time) map[string]interface{} {
	var (
		availableCount, healthyCount       float64
		totalInflight, totalRunning        float64
		totalWaiting, totalGenTPS          float64
		maxWaiting, maxKVUsage, sumKVUsage float64
		minKVUsage                         = -1.0
	)
	for _, b := range pool {
		if b.Healthy() {
			healthyCount++
		}
		if !b.Available(now) {
			continue
		}
		availableCount++
		snap := b.Snapshot()
		totalInflight += float64(b.Inflight())
		totalRunning += snap.Running
		totalWaiting += snap.Waiting
		totalGenTPS += snap.GenTokPerSec
		sumKVUsage += snap.KVUsage
		if snap.Waiting > maxWaiting {
			maxWaiting = snap.Waiting
		}
		if snap.KVUsage > maxKVUsage {
			maxKVUsage = snap.KVUsage
		}
		if minKVUsage < 0 || snap.KVUsage < minKVUsage {
			minKVUsage = snap.KVUsage
		}
	}
	avgRunning, avgWaiting, avgKVUsage := 0.0, 0.0, 0.0
	if availableCount > 0 {
		avgRunning = totalRunning / availableCount
		avgWaiting = totalWaiting / availableCount
		avgKVUsage = sumKVUsage / availableCount
	}
	if minKVUsage < 0 {
		minKVUsage = 0
	}
	return map[string]interface{}{
		"model":           model,
		"backend_count":   float64(len(pool)),
		"available_count": availableCount,
		"healthy_count":   healthyCount,
		"total_inflight":  totalInflight,
		"total_running":   totalRunning,
		"total_waiting":   totalWaiting,
		"total_gen_tps":   totalGenTPS,
		"avg_running":     avgRunning,
		"avg_waiting":     avgWaiting,
		"avg_kv_usage":    avgKVUsage,
		"max_waiting":     maxWaiting,
		"max_kv_usage":    maxKVUsage,
		"min_kv_usage":    minKVUsage,
	}
}

// numericVars 提取环境中的数值变量，作为事件的 Vars 快照。
func numericVars(env map[string]interface{}) map[string]float64 {
	out := make(map[string]float64, len(env))
	for k, v := range env {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}

// sortedKeys 返回按字典序排序的键，保证消息渲染顺序稳定。
func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
