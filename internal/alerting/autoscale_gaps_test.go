// 扩缩容缺陷回归测试：
//
// M2 失 leader 不清指标：非 leader 时 autoscale.go 的 Run 只跳过求值，
// 已产出过的建议指标（gateway_autoscale_desired_replicas）停留在陈旧值，
// HPA 会按陈旧建议继续扩容。修复后失去生产建议资格时对最近产出过建议的
// 模型逐个回调 onDesired(model, 0) 归零。
//
// M3 冷却绕开指标路径：冷却期间（未通知 webhook）onDesired 仍被调用、
// 指标被更新，与 webhook 通知不同步。修复后指标回调与通知同一门控。
package alerting

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
)

// TestAutoscaler_失leader归零指标_M2 回归 M2：
// leader 期间产出扩容建议（指标 3），失去 leader 后指标必须归零，
// 防止 HPA 按陈旧建议扩容。
func TestAutoscaler_失leader归零指标_M2(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	reg := newTestRegistry(t)
	n := NewNotifier([]config.WebhookConfig{webhookCfg("hook", srv.URL, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	desired := map[string]float64{}
	leader := atomic.Bool{}
	leader.Store(true)
	as, err := NewAutoscaler(config.AutoscaleConfig{
		Interval: config.Duration(10 * time.Millisecond),
		Webhooks: []string{"hook"},
		Policies: []config.AutoscalePolicyConfig{{
			Model: "*", MinReplicas: 1, MaxReplicas: 3,
			ScaleUpExpr: "avg_waiting > 5", ScaleUpStep: 2,
			ScaleUpCooldown: config.Duration(time.Hour),
		}},
	}, reg, n, func(model string, d float64) {
		mu.Lock()
		desired[model] = d
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("构造建议器失败: %v", err)
	}
	as.SetGate(leader.Load)

	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 10})
	reg.Get("b2").SetSnapshot(&backend.Snapshot{Waiting: 10})
	go as.Run(ctx)

	// leader 期间：产出扩容建议，指标为 3。
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return desired["*"] == 3
	}, "leader 期间应产出期望副本指标 3")

	// 失去 leader：指标归零。
	leader.Store(false)
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return desired["*"] == 0
	}, "失去 leader 后指标应归零")

	// 归零只发生一次（produced 集合已清空），指标保持 0 不回跳。
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := desired["*"]
	mu.Unlock()
	if got != 0 {
		t.Fatalf("失去 leader 后指标应保持 0,实际 %v", got)
	}
}

// TestAutoscaler_冷却绕开指标路径_M3 回归 M3：
// 冷却期内再次求值既不通知 webhook，也不应更新指标回调——
// 指标与 webhook 必须同一门控；admin 视图（a.last）仍持续更新。
func TestAutoscaler_冷却绕开指标路径_M3(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	reg := newTestRegistry(t)
	n := NewNotifier([]config.WebhookConfig{webhookCfg("hook", srv.URL, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	var mu sync.Mutex
	desiredCalls := 0
	as, err := NewAutoscaler(config.AutoscaleConfig{
		Interval: config.Duration(time.Second),
		Webhooks: []string{"hook"},
		Policies: []config.AutoscalePolicyConfig{{
			Model: "*", MinReplicas: 1, MaxReplicas: 3,
			ScaleUpExpr: "avg_waiting > 5", ScaleUpStep: 2,
			ScaleUpCooldown: config.Duration(time.Hour),
		}},
	}, reg, n, func(model string, d float64) {
		mu.Lock()
		desiredCalls++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("构造建议器失败: %v", err)
	}

	reg.Get("b1").SetSnapshot(&backend.Snapshot{Waiting: 10})
	reg.Get("b2").SetSnapshot(&backend.Snapshot{Waiting: 10})
	now := time.Now()

	// 首次求值：扩冷却未命中 -> 通知 webhook 且回调指标一次。
	as.Evaluate(now)
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "首次应收到扩容建议")

	// 冷却期内再次求值：不重复通知，onDesired 也不得再次被调用。
	as.Evaluate(now.Add(10 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if len(col.snapshot()) != 1 {
		t.Fatal("冷却期内不应重复通知")
	}
	mu.Lock()
	calls := desiredCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("修复后:冷却期内不应再次调用 onDesired,实际 %d 次", calls)
	}

	// admin 视图不受冷却门控，仍展示最近建议。
	recs := as.Recommendations()
	if len(recs) != 1 || recs[0].Direction != "up" || recs[0].DesiredReplicas != 3 {
		t.Fatalf("admin 建议视图不符: %+v", recs)
	}
}
