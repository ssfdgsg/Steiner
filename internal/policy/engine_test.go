// 策略表达式引擎单测：编译、求值、过滤、热更新与异常路径。
package policy

import (
	"strings"
	"testing"
)

// baseEnv 构造一个包含全部常用变量的求值环境。
func baseEnv(over map[string]interface{}) map[string]interface{} {
	env := map[string]interface{}{
		"model": "m", "stream": false, "prompt_len": 100.0, "priority": 0.0, "session": "",
		"backend": "b1", "engine": "vllm", "engine_family": "vllm",
		"weight": 1.0, "healthy": true, "inflight": 0.0,
		"running": 0.0, "waiting": 0.0, "kv_usage": 0.0, "hit_rate": 0.0, "gen_tps": 0.0,
		"ttft_ewma": 0.0, "preempt_rate": 0.0,
		"prefix_match": 0.0,
		"labels":       map[string]string{},
		"raw":          map[string]float64{},
		"vars":         map[string]float64{},
	}
	for k, v := range over {
		env[k] = v
	}
	return env
}

func TestSetAndEval(t *testing.T) {
	e := NewEngine()
	if err := e.Set("p", "healthy", "running * 2.0 + waiting * 5.0"); err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	p := e.Get("p")
	pass, score, err := p.Eval(baseEnv(map[string]interface{}{"running": 3.0, "waiting": 2.0}))
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if !pass {
		t.Fatal("filter 应通过")
	}
	if score != 16.0 {
		t.Fatalf("score 期望 16.0，实际 %v", score)
	}
}

func TestFilterRejects(t *testing.T) {
	e := NewEngine()
	if err := e.Set("p", "healthy && kv_usage < 0.9", "running"); err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	pass, _, err := e.Get("p").Eval(baseEnv(map[string]interface{}{"kv_usage": 0.95}))
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if pass {
		t.Fatal("kv_usage 超限应被过滤")
	}
}

func TestCompileError(t *testing.T) {
	e := NewEngine()
	err := e.Set("bad", "healthy &&", "running")
	if err == nil {
		t.Fatal("非法 filter 表达式应编译失败")
	}
	if e.Get("bad") != nil {
		t.Fatal("编译失败的策略不应注册")
	}
	if err := e.Set("bad2", "healthy", "running +"); err == nil {
		t.Fatal("非法 score 表达式应编译失败")
	}
}

func TestHotSwap(t *testing.T) {
	e := NewEngine()
	if err := e.Set("p", "", "running"); err != nil {
		t.Fatalf("初次编译失败: %v", err)
	}
	if err := e.Set("p", "", "waiting * 10.0"); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	_, score, err := e.Get("p").Eval(baseEnv(map[string]interface{}{"waiting": 2.0}))
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if score != 20.0 {
		t.Fatalf("热更新后 score 期望 20.0，实际 %v", score)
	}
}

func TestEmptyFilterDefaultsHealthy(t *testing.T) {
	e := NewEngine()
	if err := e.Set("p", "", "1.0"); err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	pass, _, err := e.Get("p").Eval(baseEnv(map[string]interface{}{"healthy": false}))
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if pass {
		t.Fatal("空 filter 缺省为 healthy，不健康后端应被过滤")
	}
}

func TestPromVarsAccess(t *testing.T) {
	e := NewEngine()
	if err := e.Set("p", "", `running + vars["gpu_util"] * 0.1`); err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	_, score, err := e.Get("p").Eval(baseEnv(map[string]interface{}{
		"running": 1.0,
		"vars":    map[string]float64{"gpu_util": 50.0},
	}))
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if score != 6.0 {
		t.Fatalf("score 期望 6.0，实际 %v", score)
	}
}

func TestMissingScore(t *testing.T) {
	e := NewEngine()
	if err := e.Set("p", "healthy", ""); err == nil || !strings.Contains(err.Error(), "score") {
		t.Fatalf("缺少 score 应报错，实际: %v", err)
	}
}

func TestList(t *testing.T) {
	e := NewEngine()
	_ = e.Set("a", "healthy", "running")
	_ = e.Set("b", "healthy", "waiting")
	l := e.List()
	if len(l) != 2 || l["a"]["score"] != "running" {
		t.Fatalf("List 输出不符: %v", l)
	}
}
