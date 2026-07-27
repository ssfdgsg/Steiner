// 外部 Prometheus 旁路采集：周期执行配置中的 PromQL 查询，
// 按标签把结果分发到各后端的表达式变量表（vars["<查询名>"]）。
// 典型用途：注入 DCGM GPU 利用率、显存占用、网卡带宽等引擎自身不暴露的指标。
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

// PromCollector 外部 Prometheus 查询器。
type PromCollector struct {
	api      promv1.API
	cfg      config.PrometheusConfig
	backends func() []*backend.Backend
}

// NewPromCollector 构造查询器；cfg.URL 为空时返回 nil 表示不启用。
// backends 为活视图函数（通常传 Registry.All），动态后端自动纳入匹配。
func NewPromCollector(cfg config.PrometheusConfig, backends func() []*backend.Backend) (*PromCollector, error) {
	if cfg.URL == "" {
		return nil, nil
	}
	client, err := api.NewClient(api.Config{Address: cfg.URL})
	if err != nil {
		return nil, err
	}
	return &PromCollector{api: promv1.NewAPI(client), cfg: cfg, backends: backends}, nil
}

// Run 周期查询，阻塞到 ctx 取消。
func (p *PromCollector) Run(ctx context.Context) {
	p.CollectOnce(ctx)
	ticker := time.NewTicker(p.cfg.Interval.D())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.CollectOnce(ctx)
		}
	}
}

// CollectOnce 执行一轮全部查询并整体替换各后端的变量表。
func (p *PromCollector) CollectOnce(ctx context.Context) {
	backends := p.backends()
	// 每轮重建，避免下线的序列残留旧值误导调度。
	perBackend := map[string]map[string]float64{}
	for _, b := range backends {
		perBackend[b.ID] = map[string]float64{}
	}

	for _, q := range p.cfg.Queries {
		qctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.D())
		val, warns, err := p.api.Query(qctx, q.Query, time.Now())
		cancel()
		if err != nil {
			slog.Warn("PromQL 查询失败", "query", q.Name, "err", err)
			continue
		}
		if len(warns) > 0 {
			slog.Debug("PromQL 查询警告", "query", q.Name, "warnings", warns)
		}
		vec, ok := val.(model.Vector)
		if !ok {
			slog.Warn("PromQL 结果不是即时向量，已跳过", "query", q.Name, "type", val.Type())
			continue
		}
		for _, sample := range vec {
			labelVal := string(sample.Metric[model.LabelName(q.BackendLabel)])
			if labelVal == "" {
				continue
			}
			for _, b := range backends {
				if p.matches(b, q.BackendLabel, labelVal) {
					perBackend[b.ID][q.Name] = float64(sample.Value)
				}
			}
		}
	}

	for _, b := range backends {
		b.SetPromVars(perBackend[b.ID])
	}
}

// matches 判定样本标签值是否指向该后端：优先比对后端自定义 labels，
// 其次比对后端地址 host:port（instance 标签的惯例格式）。
func (p *PromCollector) matches(b *backend.Backend, label, value string) bool {
	if v, ok := b.Labels[label]; ok {
		return v == value
	}
	return b.URL.Host == value
}
