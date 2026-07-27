// 网关进程入口：只做装配与生命周期管理，不含业务逻辑。
// 装配顺序：配置 → 注册表 → 策略引擎 → KV 前缀树 → 调度器 → PD 组 →
// 指标（自身导出 + 直采 + PromQL）→ 代理 → HTTP 服务；
// 后台循环（健康检查、指标抓取、树清理、规模指标刷新）随主 ctx 优雅退出。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ai-gateway/internal/alerting"
	"ai-gateway/internal/backend"
	"ai-gateway/internal/cluster"
	"ai-gateway/internal/config"
	"ai-gateway/internal/kvcache"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/reconcile"
	"ai-gateway/internal/rollout"
	"ai-gateway/internal/scheduler"
	"ai-gateway/internal/server"
	"ai-gateway/internal/session"
	"ai-gateway/internal/store"
	"ai-gateway/internal/tracing"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "配置文件路径")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("ai-gateway", version)
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(*configPath); err != nil {
		slog.Error("网关退出", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// 分布式追踪：安装全局 TracerProvider（未启用时为 noop，代理打点零开销）。
	stopTracing, err := tracing.Setup(context.Background(), cfg.Tracing, version)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stopTracing(sctx); err != nil {
			slog.Warn("追踪导出器关停失败", "err", err)
		}
	}()

	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		return err
	}

	// 策略引擎：编译配置中的全部策略，任一编译失败即启动失败（左移到部署期暴露）。
	pol := policy.NewEngine()
	for name, pc := range cfg.Policies {
		if err := pol.Set(name, pc.Filter, pc.Score); err != nil {
			return err
		}
	}

	// KV 前缀树（可关闭）。
	var tree *kvcache.Tree
	if cfg.KVCache.Enabled {
		tree = kvcache.NewTree(cfg.KVCache.MaxPrefixBytes, cfg.KVCache.TTL.D())
	}

	sched := scheduler.New(pol, tree, cfg.KVCache)

	// 动态配置持久层（可选）：连不上数据库即启动失败（左移暴露）。
	// DB 中的运行期变更（后端增删、策略热更新）覆盖 YAML 基线。
	// 必须在 pd.NewManager 之前加载：PD 组构建后持有实例指针不再刷新，
	// 若 DB 行与 PD 组成员同 ID、加载晚于组构建会产生指向旧实例的孤儿指针。
	var st *store.Store
	if cfg.Store.Enabled {
		st, err = store.Open(cfg.Store)
		if err != nil {
			return err
		}
		defer st.Close()
		loadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		rows, err := st.ListBackends(loadCtx)
		if err != nil {
			cancel()
			return err
		}
		for _, row := range rows {
			if _, err := reg.UpsertBackend(row.Backend, row.Models); err != nil {
				// 典型原因：YAML 已删除该行引用的模型路由。跳过并告警，不阻塞启动。
				slog.Warn("加载持久化后端失败，已跳过", "backend", row.Backend.ID, "err", err)
			} else {
				slog.Info("已加载持久化后端", "backend", row.Backend.ID, "models", row.Models)
			}
		}
		pols, err := st.ListPolicies(loadCtx)
		cancel()
		if err != nil {
			return err
		}
		for name, pc := range pols {
			if err := pol.Set(name, pc.Filter, pc.Score); err != nil {
				slog.Warn("加载持久化策略失败，已跳过", "policy", name, "err", err)
			} else {
				slog.Info("已加载持久化策略", "policy", name)
			}
		}
	}

	pdMgr, err := pd.NewManager(cfg, reg)
	if err != nil {
		return err
	}

	gw := metrics.NewGateway()
	gw.BuildInfo.WithLabelValues(version).Set(1)
	// 后端归一化指标透出：Prometheus 只抓网关即可获得全集群引擎负载视图。
	gw.Registry.MustRegister(metrics.NewBackendCollector(reg.All))
	// 路由级指标透出（分池分流计数）。
	gw.Registry.MustRegister(metrics.NewRouteCollector(reg.Routes()))
	px := proxy.New(cfg, reg, sched, pdMgr, gw)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 集群协调层（可选）：连不上 Redis 即启动失败（左移暴露）。
	var clu *cluster.Manager
	if cfg.Cluster.Enabled {
		clu, err = cluster.New(cfg.Cluster, cfg.Server.Listen, func(op string) {
			gw.ClusterRedisErrors.WithLabelValues(op).Inc()
		})
		if err != nil {
			return err
		}
		defer clu.Close()
		clu.OnLeaderChange(func(leader bool) {
			if leader {
				gw.ClusterLeader.Set(1)
			} else {
				gw.ClusterLeader.Set(0)
			}
		})
		go clu.Run(ctx)
		// 策略热更新广播：任一实例 admin PUT 后全集群同步。
		go clu.RunPolicySubscriber(ctx, pol.Set)
		// 后端增删广播：订阅方只改内存态，持久化由发起实例完成。
		go clu.RunBackendSubscriber(ctx, func(action string, bc config.BackendConfig, models []string) error {
			if action == cluster.BackendDelete {
				return reg.RemoveBackend(bc.ID)
			}
			_, err := reg.UpsertBackend(bc, models)
			return err
		})
		// 分布式限流：覆盖本地令牌桶，全集群共享模型配额。
		if cfg.Cluster.RateLimitMode == "distributed" {
			for _, m := range cfg.Models {
				if m.RateLimitQPS > 0 {
					px.SetLimiter(m.Name, clu.NewRateLimiter(m.Name, m.RateLimitQPS, m.RateLimitBurst))
				}
			}
		}
	}

	// 动态配置对账：以 DB 为事实来源，周期收敛错过的集群广播
	//（pub/sub 断连期间的消息不补发）。每实例独立对账，不做 leader 门控。
	if st != nil {
		go reconcile.New(st, reg, pol, cfg.Backends, cfg.Store.ReconcileInterval.D()).Run(ctx)
	}

	// 会话粘性与容量排队（均可选）。
	var sess *session.Store
	if cfg.Session.Enabled {
		sess = session.NewStore(cfg.Session.TTL.D(), cfg.Session.MaxEntries)
		if clu != nil && cfg.Cluster.SessionMode == "shared" {
			// 集群共享绑定表：会话跨网关实例仍粘到同一后端。
			px.SetSessionStore(clu.NewSessionStore(sess))
		} else {
			px.SetSessionStore(sess)
		}
	}
	var capQueue *queue.Queue
	if cfg.Queue.Enabled {
		capQueue = queue.New(cfg.Queue.MaxDepth, cfg.Queue.MaxWait.D())
		px.SetQueue(capQueue)
		// 排队深度按模型路由分列透出（抓取时实时读数）。
		gw.Registry.MustRegister(metrics.NewQueueCollector(capQueue))
		// 容量准入表达式：编译失败即启动失败（左移暴露）。
		if cfg.Queue.AdmissionExpr != "" {
			prog, err := policy.CompileBool(cfg.Queue.AdmissionExpr)
			if err != nil {
				return fmt.Errorf("排队准入表达式编译失败: %w", err)
			}
			px.SetAdmission(prog, cfg.Queue.AdmissionExpr)
		}
	}

	srv := server.New(cfg, reg, pol, sched, tree, pdMgr, gw, px)
	srv.SetCluster(clu)
	srv.SetStore(st)

	// 后台循环。健康翻转回调：下线解绑粘性会话（该后端 KV cache 已失效），
	// 恢复时唤醒排队请求（容量回来了）。
	health := backend.NewHealthChecker(reg.All, cfg.Metrics.HealthInterval.D(), cfg.Metrics.HealthTimeout.D(),
		func(b *backend.Backend, healthy bool) {
			if !healthy && sess != nil {
				sess.InvalidateBackend(b.ID)
			}
			if healthy && capQueue != nil {
				capQueue.Signal()
			}
		})
	go health.Run(ctx)

	scraper := metrics.NewScraper(reg.All, cfg.Metrics.ScrapeInterval.D(), cfg.Metrics.ScrapeTimeout.D())
	go scraper.Run(ctx)

	promCol, err := metrics.NewPromCollector(cfg.Prometheus, reg.All)
	if err != nil {
		return fmt.Errorf("初始化外部 Prometheus 查询器失败: %w", err)
	}
	if promCol != nil {
		go promCol.Run(ctx)
	}

	if tree != nil {
		go tree.RunPruner(ctx.Done(), cfg.KVCache.PruneInterval.D(), func(st kvcache.Stats) {
			gw.KVTreeBytes.Set(float64(st.Bytes))
			gw.KVTreeNodes.Set(float64(st.Nodes))
		})
	}

	// 告警规则引擎与自动扩缩容建议器：规则/表达式编译失败即启动失败（左移暴露）；
	// 建议事件经 webhook 推送给外部控制器落地，网关自身不执行扩缩容。
	// webhook 通知器为告警/扩缩容/金丝雀发布共用。
	var notifier *alerting.Notifier
	if cfg.Alerting.Enabled || cfg.Autoscale.Enabled || len(cfg.Rollouts) > 0 {
		notifier = alerting.NewNotifier(cfg.Alerting.Webhooks, func(target, outcome string) {
			gw.WebhookSent.WithLabelValues(target, outcome).Inc()
		})
		go notifier.Run(ctx)
	}
	if cfg.Alerting.Enabled || cfg.Autoscale.Enabled {
		var alertEng *alerting.Engine
		var scaler *alerting.Autoscaler
		if cfg.Alerting.Enabled {
			alertEng, err = alerting.NewEngine(cfg.Alerting, reg, notifier, func(rule string, n float64) {
				gw.AlertsFiring.WithLabelValues(rule).Set(n)
			})
			if err != nil {
				return err
			}
			if clu != nil {
				// 集群模式仅 leader 求值，避免同一告警多实例重复通知。
				alertEng.SetGate(clu.IsLeader)
			}
			go alertEng.Run(ctx)
		}
		if cfg.Autoscale.Enabled {
			scaler, err = alerting.NewAutoscaler(cfg.Autoscale, reg, notifier, func(model string, desired float64) {
				gw.AutoscaleDesired.WithLabelValues(model).Set(desired)
			})
			if err != nil {
				return err
			}
			if clu != nil {
				// 集群模式仅 leader 产出建议，避免重复扩缩容信号。
				scaler.SetGate(clu.IsLeader)
			}
			go scaler.Run(ctx)
		}
		// 管理面暴露 GET /admin/alerts 与 GET /admin/autoscale。
		srv.SetAlerting(alertEng, scaler)
	}

	// 金丝雀自动升降级：权重是每实例内存态，各实例独立执行同一份发布配置；
	// webhook 事件经 leader 门控去重。判据编译失败即启动失败（左移暴露）。
	if len(cfg.Rollouts) > 0 {
		var gate func() bool
		if clu != nil {
			gate = clu.IsLeader
		}
		ro, err := rollout.New(cfg.Rollouts, reg, notifier, gate)
		if err != nil {
			return err
		}
		px.SetResultObserver(ro)
		srv.SetRollouts(ro)
		go ro.Run(ctx)
	}

	go runGaugeUpdater(ctx, reg, pdMgr, gw)

	return srv.Run(ctx)
}

// runGaugeUpdater 周期把后端/链路运行态刷入自身指标
// （排队深度经 QueueCollector 在抓取瞬间实时读取，不在此刷新）。
func runGaugeUpdater(ctx context.Context, reg *backend.Registry, pdMgr *pd.Manager, gw *metrics.Gateway) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 后端健康/在途已并入 BackendCollector 抓取时实时透出，
			// 此处仅刷新拓扑固定的 PD 链路指标（无动态摘除残留问题）。
			for _, g := range pdMgr.Groups() {
				for _, l := range g.Links() {
					gw.PDLinkInflight.WithLabelValues(l.Prefill, l.Decode).Set(float64(l.Inflight))
				}
			}
		}
	}
}
