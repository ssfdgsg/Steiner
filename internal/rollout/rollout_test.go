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

// ——— M11：多实例 leader 单主评估 + 广播收敛 ———

// stepRecorder 记录 leader 广播的 StepEvent 序列。
type stepRecorder struct {
	mu     sync.Mutex
	events []StepEvent
}

func (s *stepRecorder) publish(_ context.Context, ev StepEvent) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}

func (s *stepRecorder) all() []StepEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StepEvent(nil), s.events...)
}

// TestM11FollowerDoesNotSelfPromote 复现并翻转：集群模式下跟随者不再以本地
// 观测自行晋级（旧缺陷：每个实例独立推进 → 无流量实例卡死、权重分叉），
// 而是由 leader 广播驱动（ApplyStep）。
func TestM11FollowerDoesNotSelfPromote(t *testing.T) {
	// 跟随者实例：判据早已满足（本地窗口喂满 + 观察期已过），但非 leader → 不晋级。
	reg := newSplitFixture(t)
	m, err := New([]config.RolloutConfig{rolloutCfg(10 * time.Millisecond)}, reg, nil, nil)
	if err != nil {
		t.Fatalf("构造发布管理器失败: %v", err)
	}
	m.SetCluster(func() bool { return false }, nil) // follower
	_, canary := splits(t, reg)
	for i := 0; i < 6; i++ {
		m.ObserveResult("b2", 200)
	}
	time.Sleep(20 * time.Millisecond)
	m.Tick(time.Now())
	if canary.Weight() != 25 {
		t.Fatalf("跟随者不应自行晋级，canary 权重应保持 25，实际 %v", canary.Weight())
	}

	// leader 实例：同配置自评估推进并广播。
	reg2 := newSplitFixture(t)
	rec := &stepRecorder{}
	leader, err := New([]config.RolloutConfig{rolloutCfg(10 * time.Millisecond)}, reg2, nil, nil)
	if err != nil {
		t.Fatalf("构造 leader 管理器失败: %v", err)
	}
	leader.SetCluster(func() bool { return true }, rec.publish)
	_, canary2 := splits(t, reg2)
	for i := 0; i < 6; i++ {
		leader.ObserveResult("b2", 200)
	}
	time.Sleep(20 * time.Millisecond)
	leader.Tick(time.Now())
	if canary2.Weight() != 100 {
		t.Fatalf("leader 应自行晋级到 100，实际 %v", canary2.Weight())
	}

	// 跟随者应用 leader 广播 → 收敛到同一权重与步进。
	evs := rec.all()
	if len(evs) == 0 {
		t.Fatal("leader 应广播发布状态变更")
	}
	for _, ev := range evs {
		if err := m.ApplyStep(context.Background(), ev); err != nil {
			t.Fatalf("ApplyStep(%+v) 失败: %v", ev, err)
		}
	}
	if canary.Weight() != 100 {
		t.Fatalf("跟随者应用广播后应收敛到 100，实际 %v", canary.Weight())
	}
}

// TestM11ApplyStepStateTransitions 跟随者状态重建：running/promoted、
// rolled_back、completed、idle 各事件的权重与状态收敛。
func TestM11ApplyStepStateTransitions(t *testing.T) {
	reg := newSplitFixture(t)
	m, err := New([]config.RolloutConfig{rolloutCfg(time.Hour)}, reg, nil, nil)
	if err != nil {
		t.Fatalf("构造发布管理器失败: %v", err)
	}
	m.SetCluster(func() bool { return false }, nil)
	stable, canary := splits(t, reg)

	// running step=1（25% → 100% 中间态按 steps[1]=100）。
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "m1", State: StateRunning, StepIdx: 1}); err != nil {
		t.Fatalf("ApplyStep running 失败: %v", err)
	}
	if canary.Weight() != 100 || stable.Weight() != 0 {
		t.Fatalf("running step1 权重不符: canary=%v stable=%v", canary.Weight(), stable.Weight())
	}
	st := m.Status()[0]
	if st.State != StateRunning || st.StepIndex != 1 {
		t.Fatalf("running 状态重建不符: %+v", st)
	}

	// rolled_back → failed + 权重清零。
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "m1", State: StateFailed, StepIdx: 1}); err != nil {
		t.Fatalf("ApplyStep failed 失败: %v", err)
	}
	if m.Status()[0].State != StateFailed || canary.Weight() != 0 || stable.Weight() != 100 {
		t.Fatalf("failed 重建不符: %+v weight canary=%v stable=%v", m.Status()[0], canary.Weight(), stable.Weight())
	}

	// completed → 末阶权重 100。
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "m1", State: StateCompleted, StepIdx: 1}); err != nil {
		t.Fatalf("ApplyStep completed 失败: %v", err)
	}
	if m.Status()[0].State != StateCompleted || canary.Weight() != 100 {
		t.Fatalf("completed 重建不符: %+v canary=%v", m.Status()[0], canary.Weight())
	}

	// idle → 等待 admin reset。
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "m1", State: StateIdle, StepIdx: 0}); err != nil {
		t.Fatalf("ApplyStep idle 失败: %v", err)
	}
	if m.Status()[0].State != StateIdle {
		t.Fatalf("idle 重建不符: %+v", m.Status()[0])
	}

	// 越界步进与未知状态拒绝。
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "m1", State: StateRunning, StepIdx: 9}); err == nil {
		t.Fatal("越界步进应报错")
	}
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "m1", State: "weird", StepIdx: 0}); err == nil {
		t.Fatal("未知状态应报错")
	}
	if err := m.ApplyStep(context.Background(), StepEvent{Model: "nope", State: StateRunning, StepIdx: 0}); err == nil {
		t.Fatal("未知模型应报错")
	}
}

// TestM11LeaderBroadcastsTransitions leader 在 started/promoted/rolled_back/
// completed 各变更点广播；单机（未 SetCluster）不广播。
func TestM11LeaderBroadcastsTransitions(t *testing.T) {
	reg := newSplitFixture(t)
	rec := &stepRecorder{}
	m, err := New([]config.RolloutConfig{rolloutCfg(20 * time.Millisecond)}, reg, nil, nil)
	if err != nil {
		t.Fatalf("构造发布管理器失败: %v", err)
	}
	m.SetCluster(func() bool { return true }, rec.publish)
	_, canary := splits(t, reg)
	_ = canary

	// started 事件：auto_start 在 New（SetCluster 前）触发、无发布者，此处以 Reset 触发。
	if err := m.Reset("m1"); err != nil {
		t.Fatalf("reset 失败: %v", err)
	}
	evs := rec.all()
	if len(evs) != 1 || evs[0].State != StateRunning || evs[0].StepIdx != 0 {
		t.Fatalf("应有 started(running,0) 广播，实际 %+v", evs)
	}

	// 喂满晋级判据 → 第一轮 tick 晋级到 running(1) → 第二轮 tick 全量 completed。
	for i := 0; i < 3; i++ {
		m.ObserveResult("b2", 200)
	}
	time.Sleep(25 * time.Millisecond)
	m.Tick(time.Now())
	evs = rec.all()
	last := evs[len(evs)-1]
	if last.State != StateRunning || last.StepIdx != 1 {
		t.Fatalf("应广播 promoted(running,1)，实际 %+v", last)
	}
	for i := 0; i < 3; i++ {
		m.ObserveResult("b2", 200)
	}
	time.Sleep(25 * time.Millisecond)
	m.Tick(time.Now())
	evs = rec.all()
	last = evs[len(evs)-1]
	if last.State != StateCompleted || last.StepIdx != 1 {
		t.Fatalf("应广播 completed(1)，实际 %+v", last)
	}

	// reset 重启 → 再次 started。
	if err := m.Reset("m1"); err != nil {
		t.Fatalf("reset 失败: %v", err)
	}
	evs = rec.all()
	last = evs[len(evs)-1]
	if last.State != StateRunning || last.StepIdx != 0 {
		t.Fatalf("reset 后应广播 started，实际 %+v", last)
	}

	// 回滚广播：金丝雀全错 → failed。
	for i := 0; i < 4; i++ {
		m.ObserveResult("b2", 502)
	}
	m.Tick(time.Now())
	evs = rec.all()
	last = evs[len(evs)-1]
	if last.State != StateFailed {
		t.Fatalf("回滚应广播 failed，实际 %+v", last)
	}

	// 单机（不 SetCluster）：不广播。
	reg2 := newSplitFixture(t)
	m2, err := New([]config.RolloutConfig{rolloutCfg(time.Hour)}, reg2, nil, nil)
	if err != nil {
		t.Fatalf("构造单机管理器失败: %v", err)
	}
	if got := len(rec.all()); got != len(evs) {
		t.Fatalf("recorder 不应再有新事件，实际 %d", got)
	}
	m2.ObserveResult("b2", 502)
	m2.Tick(time.Now())
	if got := len(rec.all()); got != len(evs) {
		t.Fatalf("单机不应广播，实际新增事件")
	}
}
