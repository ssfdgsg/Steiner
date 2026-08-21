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
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// defaultEvalTimeout 单次表达式求值的默认超时。
// 正常策略/告警/准入表达式的求值开销在百纳秒到微秒量级，100ms 远超正常
// 范围，只在病态表达式（极端迭代/深层递归）下才会触发；它防止调用方被
// 一次失控求值无限期阻塞，是兜底而非正常路径的损耗。
const defaultEvalTimeout = 100 * time.Millisecond

// RunWithTimeout 携带超时执行一次表达式求值，供本项目所有 vm.Run 求值
// 路径（策略 filter/score、告警条件、排队准入等）统一复用。
//
// 实现：在独立 goroutine 中调用 vm.Run，与定时器竞争——正常完成则返回
// 求值结果；先到超时则返回错误（错误文案含超时时长），调用方自行决定
// 错误语义（本包 Evaluate/Check 等一律按求值失败处理）。
//
// 已知取舍（expr 库限制）：expr v1.16.9 的 vm.Run 无法被中途取消，超时
// 后求值 goroutine 不会被终止，而是继续运行至自然结束（表达式是有限
// 计算，不会无限循环），仅其结果被丢弃。因此病态表达式在超时后仍会
// 占用一个 goroutine 直到跑完，热路径上应避免高频触发超时的病态表达式；
// 超时是防止调用方无限期阻塞的兜底，不是计算的终止机制。
//
// 并发安全：与 vm.Run 本身一致——每次调用在独立 goroutine 内使用新建的
// VM 实例，不共享可变状态；结果经容量 1 的缓冲通道回传，超时后 goroutine
// 的发送也不会阻塞泄漏。
func RunWithTimeout(prog *vm.Program, env any, timeout time.Duration) (any, error) {
	type evalResult struct {
		val any
		err error
	}
	done := make(chan evalResult, 1)
	go func() {
		val, err := vm.Run(prog, env)
		done <- evalResult{val: val, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.val, r.err
	case <-timer.C:
		return nil, fmt.Errorf("表达式求值超时(%v)", timeout)
	}
}

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

// ValidationError 表达式分字段编译错误，供管理面在编辑器对应区域展示。
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Validate 只编译并校验一对策略表达式，不注册、不持久化、不改变运行策略。
// filter 为空时按 Set 的真实语义归一化为 healthy；动态环境允许未知变量，
// 因此这里只保证语法与结果类型，不能保证 vars/raw 动态键在运行时存在。
func Validate(filter, score string) (normalizedFilter string, errs []ValidationError) {
	// 合法响应也稳定编码为 [] 而非 null，前端可直接遍历，无需双重判空。
	errs = make([]ValidationError, 0)
	normalizedFilter = filter
	if normalizedFilter == "" {
		normalizedFilter = "healthy"
	}
	if _, err := compile(normalizedFilter, true); err != nil {
		errs = append(errs, ValidationError{Field: "filter", Message: err.Error()})
	}
	if score == "" {
		errs = append(errs, ValidationError{Field: "score", Message: "缺少 score 表达式"})
	} else if _, err := compile(score, false); err != nil {
		errs = append(errs, ValidationError{Field: "score", Message: err.Error()})
	}
	return normalizedFilter, errs
}

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
// 求值异常（例如表达式除零、或超时）时按"不通过"处理并返回错误，
// 调度器据此跳过该后端而不是整体失败。
func (p *Policy) Eval(env map[string]interface{}) (bool, float64, error) {
	passRaw, err := RunWithTimeout(p.filterProg, env, defaultEvalTimeout)
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
	scoreRaw, err := RunWithTimeout(p.scoreProg, env, defaultEvalTimeout)
	if err != nil {
		return false, 0, fmt.Errorf("score 求值失败: %w", err)
	}
	score, ok := scoreRaw.(float64)
	if !ok {
		return false, 0, fmt.Errorf("score 结果不是 float64: %T", scoreRaw)
	}
	return true, score, nil
}
