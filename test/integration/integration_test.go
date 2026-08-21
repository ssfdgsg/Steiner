// 集群集成测试：在同一进程内装配两套完整网关（server + proxy + cluster），
// 共享一个 miniredis 与同一组 mock 后端，端到端验证跨实例行为：
// 分布式限流共享配额、会话粘性跨实例、策略广播、leader 唯一性。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/cluster"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/pd"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/scheduler"
	"ai-gateway/internal/server"
	"ai-gateway/internal/session"
	"ai-gateway/test/mockbackend"
)

// gw 一套装配完成的网关实例。
type gw struct {
	handler http.Handler
	clu     *cluster.Manager
	pol     *policy.Engine
}

// baseConfig 两个 mock 后端 + 单模型路由的基础配置；rate limit 与 session
// 由各用例按需开启。
func baseConfig(t *testing.T, redisAddr, instanceID string, backendURLs map[string]string, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Models: []config.ModelRoute{{Name: "m1", Strategy: "round_robin"}},
		Server: config.ServerConfig{AdminToken: "it-token"},
		Cluster: config.ClusterConfig{
			Enabled:           true,
			RedisAddr:         redisAddr,
			InstanceID:        instanceID,
			KeyPrefix:         "itgw",
			HeartbeatInterval: config.Duration(20 * time.Millisecond),
			HeartbeatTTL:      config.Duration(300 * time.Millisecond),
			LeaderTTL:         config.Duration(300 * time.Millisecond),
		},
	}
	for id, url := range backendURLs {
		cfg.Backends = append(cfg.Backends, config.BackendConfig{ID: id, URL: url, Engine: "vllm"})
		cfg.Models[0].Backends = append(cfg.Models[0].Backends, id)
	}
	if mutate != nil {
		mutate(cfg)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}
	return cfg
}

// newGateway 按 main.go 的装配顺序构建一套网关（省略告警/排队等无关模块）。
func newGateway(t *testing.T, ctx context.Context, cfg *config.Config) *gw {
	t.Helper()
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	pol := policy.NewEngine()
	for name, pc := range cfg.Policies {
		if err := pol.Set(name, pc.Filter, pc.Score); err != nil {
			t.Fatalf("编译策略失败: %v", err)
		}
	}
	sched := scheduler.New(pol, nil, cfg.KVCache)
	pdMgr, err := pd.NewManager(cfg, reg)
	if err != nil {
		t.Fatalf("构建 PD 管理器失败: %v", err)
	}
	gwm := metrics.NewGateway()
	px := proxy.New(cfg, reg, sched, pdMgr, gwm)

	clu, err := cluster.New(cfg.Cluster, cfg.Server.Listen, nil)
	if err != nil {
		t.Fatalf("构建集群管理器失败: %v", err)
	}
	t.Cleanup(func() { _ = clu.Close() })
	go clu.Run(ctx)
	go clu.RunPolicySubscriber(ctx, pol.Set)
	go clu.RunBackendSubscriber(ctx, func(action string, bc config.BackendConfig, models []string) error {
		if action == cluster.BackendDelete {
			return reg.RemoveBackend(bc.ID)
		}
		_, err := reg.UpsertBackend(bc, models)
		return err
	})
	if cfg.Cluster.RateLimitMode == "distributed" {
		for _, m := range cfg.Models {
			if m.RateLimitQPS > 0 {
				px.SetLimiter(m.Name, clu.NewRateLimiter(m.Name, m.RateLimitQPS, m.RateLimitBurst))
			}
		}
	}
	if cfg.Session.Enabled {
		local := session.NewStore(cfg.Session.TTL.D(), cfg.Session.MaxEntries)
		px.SetSessionStore(clu.NewSessionStore(local))
	}

	srv := server.New(cfg, reg, pol, sched, nil, pdMgr, gwm, px)
	srv.SetCluster(clu)
	return &gw{handler: srv.Handler(), clu: clu, pol: pol}
}

// twoGateways 起 miniredis + 两个 mock 后端 + 两套网关。
func twoGateways(t *testing.T, mutate func(*config.Config)) (*gw, *gw) {
	t.Helper()
	mr := miniredis.RunT(t)

	urls := map[string]string{}
	for _, id := range []string{"b1", "b2"} {
		m := mockbackend.New("vllm")
		s := httptest.NewServer(m.Handler())
		t.Cleanup(s.Close)
		urls[id] = s.URL
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g1 := newGateway(t, ctx, baseConfig(t, mr.Addr(), "gw-1", urls, mutate))
	g2 := newGateway(t, ctx, baseConfig(t, mr.Addr(), "gw-2", urls, mutate))

	// pub/sub 无重放：等两个实例的策略/后端订阅都建立后再返回，
	// 避免用例先发布导致广播消息丢失的偶发失败。
	probe := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = probe.Close() })
	waitFor(t, 2*time.Second, func() bool {
		n, err := probe.PubSubNumSub(ctx, "itgw:policy", "itgw:backends").Result()
		return err == nil && n["itgw:policy"] >= 2 && n["itgw:backends"] >= 2
	}, "集群订阅未就绪")
	return g1, g2
}

// doChat 向指定网关发一次非流式补全，返回状态码与 X-Upstream-Backend。
func doChat(g *gw, sessionID string) (int, string) {
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "m1",
		"messages": []map[string]string{{"role": "user", "content": "你好"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	g.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("X-Upstream-Backend")
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestRateLimitSharedAcrossInstances 分布式限流：两实例共享同一份 GCRA 配额，
// 瞬时 20 连发（交替打到两个网关）只应放行约 burst 个。
func TestRateLimitSharedAcrossInstances(t *testing.T) {
	g1, g2 := twoGateways(t, func(c *config.Config) {
		c.Models[0].RateLimitQPS = 5
		c.Models[0].RateLimitBurst = 5
	})

	allowed := 0
	for i := 0; i < 20; i++ {
		g := g1
		if i%2 == 1 {
			g = g2
		}
		if code, _ := doChat(g, ""); code == http.StatusOK {
			allowed++
		}
	}
	if allowed < 4 || allowed > 6 {
		t.Fatalf("期望全集群放行约 5 个（burst），实际 %d（本地限流会放行约 10 个）", allowed)
	}
}

// TestSessionStickyAcrossInstances 会话粘性共享：gw-1 上建立的绑定，
// 同一会话经 gw-2 进入仍应粘到同一后端（round_robin 本会交替）。
func TestSessionStickyAcrossInstances(t *testing.T) {
	g1, g2 := twoGateways(t, func(c *config.Config) {
		c.Session.Enabled = true
	})

	code, first := doChat(g1, "sess-x")
	if code != http.StatusOK || first == "" {
		t.Fatalf("首请求失败: code=%d backend=%q", code, first)
	}
	for i := 0; i < 6; i++ {
		g := g1
		if i%2 == 1 {
			g = g2
		}
		code, got := doChat(g, "sess-x")
		if code != http.StatusOK {
			t.Fatalf("第 %d 次请求失败: %d", i, code)
		}
		if got != first {
			t.Fatalf("第 %d 次请求漂移到 %s（应粘在 %s）", i, got, first)
		}
	}
}

// TestPolicyBroadcastAcrossInstances 策略广播：gw-1 的 admin PUT 应同步到 gw-2。
func TestPolicyBroadcastAcrossInstances(t *testing.T) {
	g1, g2 := twoGateways(t, nil)

	body := `{"filter":"healthy","score":"waiting * 3.0"}`
	req := httptest.NewRequest("PUT", "/admin/policies/it-policy", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer it-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g1.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 策略失败: %d %s", rec.Code, rec.Body.String())
	}

	waitFor(t, 2*time.Second, func() bool {
		p, ok := g2.pol.List()["it-policy"]
		return ok && p["score"] == "waiting * 3.0"
	}, "gw-2 未收到策略广播")
}

// TestSingleLeader 选举：两实例中恰好一个持有 leader 租约。
func TestSingleLeader(t *testing.T) {
	g1, g2 := twoGateways(t, nil)
	waitFor(t, 2*time.Second, func() bool {
		return g1.clu.IsLeader() != g2.clu.IsLeader() &&
			(g1.clu.IsLeader() || g2.clu.IsLeader())
	}, "未能选出唯一 leader")
}
