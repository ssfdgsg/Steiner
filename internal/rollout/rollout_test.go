// 金丝雀自动升降级单测：阶梯晋级、自动回滚、reset 重启、权重分摊与事件推送。
package rollout

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/alerting"
	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

// newSplitFixture 构造 stable(b1, 权重90) + canary(b2, 权重10) 的分池路由。
func newSplitFixture(t *testing.T) *backend.Registry {
	t.Helper()
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "vllm"},
		},
		Models: []config.ModelRoute{{Name: "m1", Splits: []config.SplitConfig{
			{Name: "stable", Backends: []string{"b1"}, Weight: 90},
			{Name: "canary", Backends: []string{"b2"}, Weight: 10},
		}}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	return reg
}

func rolloutCfg(interval time.Duration) config.RolloutConfig {
	auto := true
	return config.RolloutConfig{
		Model: "m1", Canary: "canary",
		Steps:        []float64{25, 100},
		Interval:     config.Duration(interval),
		PromoteExpr:  "canary_requests >= 3 && canary_error_rate < 0.5",
		RollbackExpr: "canary_requests >= 3 && canary_error_rate > 0.5",
		AutoStart:    &auto,
	}
}

// splits 取路由的 stable/canary 子池。
func splits(t *testing.T, reg *backend.Registry) (stable, canary *backend.Split) {
	t.Helper()
	rt, err := reg.Route("m1")
	if err != nil {
		t.Fatalf("查路由失败: %v", err)
	}
	for _, sp := range rt.Splits {
		switch sp.Name {
		case "stable":
			stable = sp
		case "canary":
			canary = sp
		}
	}
	return stable, canary
}

// eventSink 收集 webhook 事件。
type eventSink struct {
	mu     sync.Mutex
	events []alerting.Event
}

func (s *eventSink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ev alerting.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (s *eventSink) statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, ev := range s.events {
		out[i] = ev.Status
	}
	return out
}

// TestPromoteToCompletion 验证：启动即首阶权重生效 → 判据满足逐阶晋级 → 全量完成。
func TestPromoteToCompletion(t *testing.T) {
	reg := newSplitFixture(t)
	sink := &eventSink{}
	srv := httptest.NewServer(sink.handler())
	defer srv.Close()
	notifier := alerting.NewNotifier([]config.WebhookConfig{{
		Name: "w", URL: srv.URL, Template: "generic",
		Timeout: config.Duration(time.Second), Retries: 1, RetryBackoff: config.Duration(time.Millisecond),
	}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Run(ctx)

	rc := rolloutCfg(20 * time.Millisecond)
	rc.Webhooks = []string{"w"}
	m, err := New([]config.RolloutConfig{rc}, reg, notifier, nil)
	if err != nil {
		t.Fatalf("构造发布管理器失败: %v", err)
	}
	stable, canary := splits(t, reg)

	// 启动：首阶 25%，稳定池分摊 75%。
	if canary.Weight() != 25 || stable.Weight() != 75 {
		t.Fatalf("首阶权重不符: canary=%v stable=%v", canary.Weight(), stable.Weight())
	}

	// 喂 3 个成功结果，观察期未满不晋级。
	for i := 0; i < 3; i++ {
		m.ObserveResult("b2", 200)
		m.ObserveTTFT("b2", 0.05)
	}
	m.Tick(time.Now())
	if canary.Weight() != 25 {
		t.Fatal("观察期未满不应晋级")
	}

	// 观察期满 → 晋级到 100 → 稳定池 0。
	time.Sleep(25 * time.Millisecond)
	m.Tick(time.Now())
	if canary.Weight() != 100 || stable.Weight() != 0 {
		t.Fatalf("应晋级到全量: canary=%v stable=%v", canary.Weight(), stable.Weight())
	}

	// 第二阶观察期满且判据满足 → completed。
	for i := 0; i < 3; i++ {
		m.ObserveResult("b2", 200)
	}
	time.Sleep(25 * time.Millisecond)
	m.Tick(time.Now())
	st := m.Status()[0]
	if st.State != StateCompleted {
		t.Fatalf("应进入 completed: %+v", st)
	}

	waitEvents(t, sink, 3) // started + promoted + completed
	got := sink.statuses()
	if got[0] != "started" || got[1] != "promoted" || got[2] != "completed" {
		t.Fatalf("事件序列不符: %v", got)
	}
}

// TestRollbackAndReset 验证：错误率越界立即回滚（不等观察期）→ reset 重启。
func TestRollbackAndReset(t *testing.T) {
	reg := newSplitFixture(t)
	m, err := New([]config.RolloutConfig{rolloutCfg(time.Hour)}, reg, nil, nil)
	if err != nil {
		t.Fatalf("构造发布管理器失败: %v", err)
	}
	stable, canary := splits(t, reg)

	// 金丝雀全错：观察期远未满（1h），回滚仍应立即发生。
	for i := 0; i < 4; i++ {
		m.ObserveResult("b2", 502)
	}
	m.Tick(time.Now())
	if canary.Weight() != 0 || stable.Weight() != 100 {
		t.Fatalf("回滚后权重不符: canary=%v stable=%v", canary.Weight(), stable.Weight())
	}
	if m.Status()[0].State != StateFailed {
		t.Fatalf("应进入 failed: %+v", m.Status()[0])
	}

	// failed 状态不再推进。
	m.Tick(time.Now())
	if canary.Weight() != 0 {
		t.Fatal("failed 状态不应再调权重")
	}

	// reset 重启：回到首阶，窗口清零（旧错误不再触发回滚）。
	if err := m.Reset("m1"); err != nil {
		t.Fatalf("reset 失败: %v", err)
	}
	if canary.Weight() != 25 || m.Status()[0].State != StateRunning {
		t.Fatalf("reset 后应回到首阶: weight=%v state=%s", canary.Weight(), m.Status()[0].State)
	}
	m.Tick(time.Now())
	if m.Status()[0].State != StateRunning {
		t.Fatal("窗口已清零，旧错误不应再触发回滚")
	}
}

func waitEvents(t *testing.T, sink *eventSink, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		got := len(sink.events)
		sink.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个事件超时，实际 %v", n, sink.statuses())
}
