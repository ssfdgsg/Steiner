// 复现性单测:覆盖缺陷回归(M1: 告警表达式求值错误必须不驱动状态机,
// 否则 firing 实例会被误发 resolved 并删除状态、形成触发-误恢复抖动)。
// 修复前本测试断言缺陷行为,修复后断言正确行为。
package alerting

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

// TestEngine_求值错误误发Resolved_M1 回归 M1:
//
// 规则表达式 float(labels.env) > 0.5 引用的后端标签在运行期变为不可解析的
// 非数字字符串,求值从"为真"转为 vm.Run 运行期报错。修复前 rules.go 的
// evalInstance 把求值错误按"不满足"处理(rules.go:153-158),对正 firing 的
// 实例误发 StatusResolved 并删除其状态(rules.go:165-171);修复后求值错误
// 不驱动状态机——保留 firing 状态、不发任何事件,保留下轮重估机会。
//
// 注:审查报告列举的触发方式(缺失动态键/除零)在本仓库 expr v1.16.9 +
// expr.Env(map[string]interface{}{}) + AllowUndefinedVariables 的编译配置下
// 并不报错(缺失键取零值、浮点除零得 +Inf),此处沿用"运行期类型转换错误"
// (float() 解析非数字字符串)这一同样真实可触发的求值错误形态。
func TestEngine_求值错误误发Resolved_M1(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	reg := newTestRegistry(t)
	n := NewNotifier([]config.WebhookConfig{webhookCfg("w1", srv.URL, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	eng, err := NewEngine(config.AlertingConfig{
		Interval: config.Duration(time.Second),
		Rules: []config.AlertRuleConfig{{
			Name: "label_ratio", Scope: "backend", Expr: "float(labels.env) > 0.5",
			For: config.Duration(30 * time.Millisecond), Severity: "warning",
		}},
	}, reg, n, nil)
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}

	// b1 的 env 标签为可解析数字 → 表达式为真;b2 为 0 值不参与。
	reg.Get("b1").Labels = map[string]string{"env": "0.9"}
	reg.Get("b2").Labels = map[string]string{"env": "0"}
	now := time.Now()
	eng.Evaluate(now)
	if len(col.snapshot()) != 0 {
		t.Fatal("pending 阶段不应发送通知")
	}
	active := eng.Active()
	if len(active) != 1 || active[0].State != "pending" || active[0].Instance != "b1" {
		t.Fatalf("应有一条 pending 告警: %+v", active)
	}

	// 越过 For 时长 → firing,收到 firing 事件。
	eng.Evaluate(now.Add(50 * time.Millisecond))
	waitFor(t, 2*time.Second, func() bool { return len(col.snapshot()) == 1 }, "应收到 firing 事件")
	if ev := col.snapshot()[0]; ev.Status != StatusFiring {
		t.Fatalf("第一条事件应为 firing: %+v", ev)
	}

	// 关键步:env 标签在运行期变为非数字字符串,float() 求值报错。
	// 修复后:求值错误不驱动状态机——不改变 firing 状态、不删除状态、
	// 不发任何事件,保留该实例为 firing 等待下轮重估。
	reg.Get("b1").Labels["env"] = "abc"
	eng.Evaluate(now.Add(100 * time.Millisecond))
	// 留出异步投递窗口:修复后不应有任何第二条事件到达。
	time.Sleep(100 * time.Millisecond)
	if n := len(col.snapshot()); n != 1 {
		t.Fatalf("修复后:求值错误不应产生新事件,实际收到 %d 条: %+v", n, col.snapshot())
	}
	if ev := col.snapshot()[0]; ev.Status != StatusFiring {
		t.Fatalf("仅存的事件仍应为 firing: %+v", ev)
	}
	active = eng.Active()
	if len(active) != 1 || active[0].State != "firing" || active[0].Instance != "b1" {
		t.Fatalf("修复后:求值错误应保留 firing 状态,实际 %+v", active)
	}
}
