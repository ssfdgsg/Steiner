// Package policy 实现调度策略表达式的动态编译与求值。
// 表达式基于 expr-lang/expr 编译为字节码程序，求值开销在百纳秒量级，
// 支持运行期热更新（admin API 提交新表达式后原子替换）。
//
// 每条策略由两个表达式组成：
//   - filter：bool，false 表示该后端不参与本次调度；
//   - score：数值，分数最小者胜出（把 score 理解为"代价"）。
//
// 可用变量见 proxy/scheduler 构建的求值环境（README 有完整变量表）。
package policy

import (
	"fmt"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// Policy 一条已编译的策略。
type Policy struct {
	Name      string
	FilterSrc string
	ScoreSrc  string

	filterProg *vm.Program
	scoreProg  *vm.Program
}

// Engine 策略引擎，持有全部已编译策略，读多写少用 RWMutex。
type Engine struct {
	mu       sync.RWMutex
	policies map[string]*Policy
}

// NewEngine 构造空引擎。
func NewEngine() *Engine {
	return &Engine{policies: map[string]*Policy{}}
}

// compileOpts 编译选项：环境为动态 map，允许未知变量（运行期由求值环境兜底），
// 输出类型分别约束为 bool / float64。
func compile(src string, asBool bool) (*vm.Program, error) {
	opts := []expr.Option{
		expr.Env(map[string]interface{}{}),
		expr.AllowUndefinedVariables(),
	}
	if asBool {
		opts = append(opts, expr.AsBool())
	} else {
		opts = append(opts, expr.AsFloat64())
	}
	return expr.Compile(src, opts...)
}

// CompileBool 编译布尔表达式（选项与调度策略一致：动态环境 + 未知变量兜底）。
// 供告警规则、扩缩容判据、排队准入等全部布尔表达式场景共用，
// 保证全项目表达式方言与编译行为只有一处定义。
func CompileBool(src string) (*vm.Program, error) { return compile(src, true) }

// Set 编译并注册（或替换）一条策略。filter 为空默认 "healthy"。
func (e *Engine) Set(name, filter, score string) error {
	if score == "" {
		return fmt.Errorf("策略 %s 缺少 score 表达式", name)
	}
	if filter == "" {
		filter = "healthy"
	}
	fp, err := compile(filter, true)
	if err != nil {
		return fmt.Errorf("策略 %s 的 filter 编译失败: %w", name, err)
	}
	sp, err := compile(score, false)
	if err != nil {
		return fmt.Errorf("策略 %s 的 score 编译失败: %w", name, err)
	}
	p := &Policy{Name: name, FilterSrc: filter, ScoreSrc: score, filterProg: fp, scoreProg: sp}
	e.mu.Lock()
	e.policies[name] = p
	e.mu.Unlock()
	return nil
}

// Get 按名取策略，不存在返回 nil。
func (e *Engine) Get(name string) *Policy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.policies[name]
}

// List 返回全部策略的源码视图（用于 admin API 展示）。
func (e *Engine) List() map[string]map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]map[string]string, len(e.policies))
	for name, p := range e.policies {
		out[name] = map[string]string{"filter": p.FilterSrc, "score": p.ScoreSrc}
	}
	return out
}

// Eval 对单个后端求值：返回是否通过过滤、代价分数。
// 求值异常（例如表达式除零）时按"不通过"处理并返回错误，
// 调度器据此跳过该后端而不是整体失败。
func (p *Policy) Eval(env map[string]interface{}) (bool, float64, error) {
	passRaw, err := vm.Run(p.filterProg, env)
	if err != nil {
		return false, 0, fmt.Errorf("filter 求值失败: %w", err)
	}
	pass, ok := passRaw.(bool)
	if !ok {
		return false, 0, fmt.Errorf("filter 结果不是 bool: %T", passRaw)
	}
	if !pass {
		return false, 0, nil
	}
	scoreRaw, err := vm.Run(p.scoreProg, env)
	if err != nil {
		return false, 0, fmt.Errorf("score 求值失败: %w", err)
	}
	score, ok := scoreRaw.(float64)
	if !ok {
		return false, 0, fmt.Errorf("score 结果不是 float64: %T", scoreRaw)
	}
	return true, score, nil
}
