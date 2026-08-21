package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

// collector 收集 webhook 收到的事件，并可注入前 N 次失败以测试重试。
type collector struct {
	mu       sync.Mutex
	events   []Event
	failLeft int
}

func (c *collector) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.failLeft > 0 {
			c.failLeft--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.events = append(c.events, ev)
		w.WriteHeader(http.StatusOK)
	}
}

func (c *collector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

// waitFor 轮询等待条件成立，超时报错。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// newTestRegistry 构造含两个后端与 "*" 路由的注册表。
func newTestRegistry(t *testing.T) *backend.Registry {
	t.Helper()
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "b1", URL: "http://127.0.0.1:1", Engine: "vllm"},
			{ID: "b2", URL: "http://127.0.0.1:2", Engine: "sglang"},
		},
		Models: []config.ModelRoute{{Name: "*", Backends: []string{"b1", "b2"}}},
	}
	cfg.ApplyDefaults()
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	return reg
}

func webhookCfg(name, url string, failRetries int) config.WebhookConfig {
	return config.WebhookConfig{
		Name: name, URL: url, Template: "generic",
		Timeout: config.Duration(time.Second), Retries: failRetries,
		RetryBackoff: config.Duration(5 * time.Millisecond),
	}
}

// TestNotifier_投递与重试 验证 generic 模板投递、前两次 5xx 后重试成功。
func TestNotifier_投递与重试(t *testing.T) {
	col := &collector{failLeft: 2}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	n := NewNotifier([]config.WebhookConfig{webhookCfg("w1", srv.URL, 3)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Send(Event{Type: TypeAlert, Status: StatusFiring, Rule: "r1", Instance: "b1"}, nil)
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "事件应在重试后送达")

	got := col.snapshot()[0]
	if got.Rule != "r1" || got.Status != StatusFiring {
		t.Fatalf("事件内容不符: %+v", got)
	}
}

// TestNotifier_模板渲染 验证钉钉模板的消息结构。
func TestNotifier_模板渲染(t *testing.T) {
	body, err := renderTemplate("dingtalk", Event{
		Type: TypeAlert, Status: StatusFiring, Rule: "high_waiting",
		Severity: "critical", Instance: "b1", Expr: "waiting > 10",
		Vars: map[string]float64{"waiting": 42},
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("消息不是合法 JSON: %v", err)
	}
	if msg["msgtype"] != "markdown" {
		t.Fatalf("钉钉消息类型应为 markdown: %v", msg["msgtype"])
	}
	if _, err := renderTemplate("不存在", Event{}); err == nil {
		t.Fatal("未知模板应报错")
	}
}

// TestEngine_状态机 验证 pending -> firing -> resolved 全流程与 firing 计数回调。
func TestEngine_状态机(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	reg := newTestRegistry(t)
	n := NewNotifier([]config.WebhookConfig{webhookCfg("w1", srv.URL, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	var mu sync.Mutex
	firingCounts := map[string]float64{}
	eng, err := NewEngine(config.AlertingConfig{
		Interval: config.Duration(time.Second),
		Rules: []config.AlertRuleConfig{{
			Name: "high_waiting", Scope: "backend", Expr: "waiting > 10",
			For: config.Duration(30 * time.Millisecond), Severity: "critical",
		}},
	}, reg, n, func(rule string, c float64) {
		mu.Lock()
		firingCounts[rule] = c
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}

	// 第一轮：b1 超阈值 -> pending，不应通知。
	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 20})
	now := time.Now()
	eng.Evaluate(now)
	if len(col.snapshot()) != 0 {
		t.Fatal("pending 阶段不应发送通知")
	}
	active := eng.Active()
	if len(active) != 1 || active[0].State != "pending" || active[0].Instance != "b1" {
		t.Fatalf("应有一条 pending 告警: %+v", active)
	}

	// 第二轮：越过 For 时长 -> firing，收到 firing 事件。
	eng.Evaluate(now.Add(50 * time.Millisecond))
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "应收到 firing 事件")
	if ev := col.snapshot()[0]; ev.Status != StatusFiring || ev.Instance != "b1" || ev.Vars["waiting"] != 20 {
		t.Fatalf("firing 事件内容不符: %+v", ev)
	}
	mu.Lock()
	if firingCounts["high_waiting"] != 1 {
		mu.Unlock()
		t.Fatalf("firing 计数应为 1: %v", firingCounts)
	}
	mu.Unlock()

	// 第三轮：恢复正常 -> resolved 事件，活动告警清空。
	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 0})
	eng.Evaluate(now.Add(100 * time.Millisecond))
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 2 }, "应收到 resolved 事件")
	if ev := col.snapshot()[1]; ev.Status != StatusResolved {
		t.Fatalf("第二条事件应为 resolved: %+v", ev)
	}
	if len(eng.Active()) != 0 {
		t.Fatal("resolved 后活动告警应清空")
	}
}

// TestEngine_集群作用域 验证 cluster 规则按模型路由聚合求值。
func TestEngine_集群作用域(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	reg := newTestRegistry(t)
	n := NewNotifier([]config.WebhookConfig{webhookCfg("w1", srv.URL, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	eng, err := NewEngine(config.AlertingConfig{
		Rules: []config.AlertRuleConfig{{
			Name: "cluster_pressure", Scope: "cluster",
			Expr: "avg_waiting > 5 && available_count >= 2", Severity: "warning",
		}},
	}, reg, n, nil)
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}

	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 8})
	reg.Get("b2").SetSnapshot(&backend.Snapshot{Waiting: 6})
	eng.Evaluate(time.Now())
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "集群规则应触发")
	ev := col.snapshot()[0]
	if ev.Instance != "*" || ev.Vars["avg_waiting"] != 7 {
		t.Fatalf("集群事件内容不符: %+v", ev)
	}
}

// TestAutoscaler_扩容建议与冷却 验证建议产出、钳位与冷却抑制。
func TestAutoscaler_扩容建议与冷却(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	reg := newTestRegistry(t)
	n := NewNotifier([]config.WebhookConfig{webhookCfg("hook", srv.URL, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	var mu sync.Mutex
	desired := map[string]float64{}
	as, err := NewAutoscaler(config.AutoscaleConfig{
		Interval: config.Duration(time.Second),
		Webhooks: []string{"hook"},
		Policies: []config.AutoscalePolicyConfig{{
			Model: "*", MinReplicas: 1, MaxReplicas: 3,
			ScaleUpExpr: "avg_waiting > 5", ScaleDownExpr: "avg_waiting < 1",
			ScaleUpStep: 2, ScaleDownStep: 1,
			ScaleUpCooldown:   config.Duration(time.Hour),
			ScaleDownCooldown: config.Duration(time.Hour),
		}},
	}, reg, n, func(model string, d float64) {
		mu.Lock()
		desired[model] = d
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("构造建议器失败: %v", err)
	}

	// 高负载 -> 扩容建议：2 + 2 钳位到 max 3。
	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 10})
	reg.Get("b2").SetSnapshot(&backend.Snapshot{Waiting: 10})
	now := time.Now()
	as.Evaluate(now)
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "应收到扩容建议")
	ev := col.snapshot()[0]
	if ev.Type != TypeAutoscale || ev.Scale == nil {
		t.Fatalf("事件类型不符: %+v", ev)
	}
	if ev.Scale.Direction != "up" || ev.Scale.DesiredReplicas != 3 || ev.Scale.CurrentReplicas != 2 {
		t.Fatalf("扩容建议不符: %+v", ev.Scale)
	}
	mu.Lock()
	if desired["*"] != 3 {
		mu.Unlock()
		t.Fatalf("期望副本指标应为 3: %v", desired)
	}
	mu.Unlock()

	// 冷却期内再次求值：不重复通知，但 admin 视图持续更新。
	as.Evaluate(now.Add(10 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if len(col.snapshot()) != 1 {
		t.Fatal("冷却期内不应重复通知")
	}
	recs := as.Recommendations()
	if len(recs) != 1 || recs[0].Direction != "up" || recs[0].DesiredReplicas != 3 {
		t.Fatalf("admin 建议视图不符: %+v", recs)
	}

	// 负载归零 -> 缩容方向（独立冷却，首次可发）：2 - 1 = 1。
	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 0})
	reg.Get("b2").SetSnapshot(&backend.Snapshot{Waiting: 0})
	as.Evaluate(now.Add(20 * time.Second))
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 2 }, "应收到缩容建议")
	if ev := col.snapshot()[1]; ev.Scale.Direction != "down" || ev.Scale.DesiredReplicas != 1 {
		t.Fatalf("缩容建议不符: %+v", ev.Scale)
	}
}

// TestNotifier_重复目标去重_L8 验证同一 webhook 目标被重复引用时只投递一次
// （运行时按 URL 身份去重，保持首次出现顺序）：异名同 URL 与同名重复都不重复投递。
func TestNotifier_重复目标去重_L8(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	// 配置冗余：w1/w2 指向同一 URL（异名同 URL），另有独立目标 w3。
	n := NewNotifier([]config.WebhookConfig{
		webhookCfg("w1", srv.URL, 0),
		webhookCfg("w2", srv.URL, 0),
		webhookCfg("w3", srv.URL, 0), // 同 URL 第三个名字，验证多条目去重
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	// 事件同时投递到 w1/w2/w3（三者同 URL）→ 只应送达一次。
	n.Send(Event{Type: TypeAlert, Status: StatusFiring, Rule: "r1", Instance: "b1"}, []string{"w1", "w2", "w3"})
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "同 URL 重复目标应只投递一次")
	time.Sleep(50 * time.Millisecond)
	if got := col.snapshot(); len(got) != 1 {
		t.Fatalf("L8 修复后:同 URL 重复目标只应投递一次,实际 %d 条: %+v", len(got), got)
	}

	// 同名重复引用（规则列表里同一名字写多次）同样只投递一次。
	n.Send(Event{Type: TypeAlert, Status: StatusFiring, Rule: "r2", Instance: "b1"}, []string{"w1", "w1"})
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 2 }, "同名重复应只多投递一次")
	time.Sleep(50 * time.Millisecond)
	got := col.snapshot()
	if len(got) != 2 {
		t.Fatalf("L8 修复后:同名重复只应多投递一次,实际累计 %d 条: %+v", len(got), got)
	}
	if got[0].Rule != "r1" || got[1].Rule != "r2" {
		t.Fatalf("事件顺序/内容不符: %+v", got)
	}
}

// TestConfig_告警段校验 验证新增配置段的默认值与非法引用拦截。
func TestConfig_告警段校验(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Backends: []config.BackendConfig{{ID: "b1", URL: "http://x", Engine: "vllm"}},
			Models:   []config.ModelRoute{{Name: "*", Backends: []string{"b1"}}},
		}
	}

	// 合法配置：默认值填充。
	c := base()
	c.Alerting = config.AlertingConfig{
		Webhooks: []config.WebhookConfig{{Name: "w", URL: "http://hook"}},
		Rules:    []config.AlertRuleConfig{{Name: "r", Expr: "waiting > 1"}},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if c.Alerting.Webhooks[0].Template != "generic" || c.Alerting.Rules[0].Scope != "backend" {
		t.Fatalf("默认值未填充: %+v", c.Alerting)
	}

	// 规则引用不存在的 webhook。
	c = base()
	c.Alerting = config.AlertingConfig{
		Rules: []config.AlertRuleConfig{{Name: "r", Expr: "true", Webhooks: []string{"缺失"}}},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("引用不存在的 webhook 应报错")
	}

	// autoscale 引用不存在的模型路由。
	c = base()
	c.Autoscale = config.AutoscaleConfig{
		Enabled:  true,
		Policies: []config.AutoscalePolicyConfig{{Model: "不存在", ScaleUpExpr: "true"}},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("引用不存在的模型路由应报错")
	}
}

// ——— M13：leader 翻转/重启后的重复 firing 通知去重 ———

// TestM13FiringDedupeWindow 连续 firing（无 resolved 间隔）在窗口内只通知
// 一条；resolved 重置窗口，之后的 firing 重新生效。
func TestM13FiringDedupeWindow(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	n := NewNotifier([]config.WebhookConfig{webhookCfg("w1", srv.URL, 0)}, nil)
	n.SetDedupeWindow(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	fire := func() {
		n.Send(Event{Type: TypeAlert, Rule: "r1", Instance: "b1", Status: StatusFiring, Summary: "s"}, []string{"w1"})
	}
	resolve := func() {
		n.Send(Event{Type: TypeAlert, Rule: "r1", Instance: "b1", Status: StatusResolved, Summary: "s"}, []string{"w1"})
	}

	// 第一次 firing → 收到。
	fire()
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "首次 firing 应送达")
	// 窗口内重复 firing（模拟 leader 翻转/重启后新实例重建状态再 fire）→ 去重。
	fire()
	time.Sleep(30 * time.Millisecond)
	if got := len(col.snapshot()); got != 1 {
		t.Fatalf("窗口内重复 firing 应被去重，实际收到 %d 条", got)
	}
	// resolved → 送达（不能因去重丢失解决通知）。
	resolve()
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 2 }, "resolved 应送达")
	// resolved 重置窗口：再次 firing 重新生效。
	fire()
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 3 }, "resolved 后 firing 应重新生效")
}

// TestM13DedupeWindowDisabled 窗口为 0（关闭）时重复 firing 照常送达
// （兼容启用了 repeat_interval 需要高频重发的规则）。
func TestM13DedupeWindowDisabled(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	n := NewNotifier([]config.WebhookConfig{webhookCfg("w1", srv.URL, 0)}, nil) // 默认窗口 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	for i := 0; i < 2; i++ {
		n.Send(Event{Type: TypeAlert, Rule: "r1", Instance: "b1", Status: StatusFiring, Summary: "s"}, []string{"w1"})
	}
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 2 }, "窗口关闭时两条 firing 均应送达")
}
