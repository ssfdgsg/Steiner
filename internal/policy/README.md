# internal/policy/ — 策略表达式引擎

## 职责
把配置或管理面提交的**调度算式**（字符串）动态编译为可执行程序，供调度器在热路径上
对每个候选后端求值。核心特性：

- **动态编译**：基于 `expr-lang/expr` 编译为字节码（`vm.Program`），求值在百纳秒量级；
- **热更新**：`PUT /admin/policies/{name}` 编译成功才原子替换，失败返回 400 且运行策略不变；
- **安全降级**：单后端求值异常（如除零）只跳过该后端；策略过滤后无候选时调度器
  降级为最小负载（`least_request` 语义），保证请求不因策略过严而整体失败。

## 表达式模型
一条策略 = `filter`（bool，false 淘汰候选；空串缺省为 `healthy`）
+ `score`（数值，**分数最小者胜出**，把 score 理解为"代价"）。

```yaml
policies:
  default:
    filter: "healthy && kv_usage < 0.98"
    score: "running * 2.0 + waiting * 6.0 + inflight * 1.0 + kv_usage * 8.0 - prefix_match * 10.0"
```

## 变量表（求值环境，唯一权威来源；实现见 scheduler.BuildEnv）

| 变量 | 类型 | 说明 |
|---|---|---|
| `model` | string | 请求模型名 |
| `stream` | bool | 是否流式请求 |
| `prompt_len` | float | 提示词文本字节长度 |
| `priority` | float | 请求体 `priority` 字段（缺省 0） |
| `session` | string | 会话键（X-Session-Id 头或请求体 user 字段） |
| `backend` | string | 候选后端 ID |
| `engine` | string | vllm / vllm_omni / sglang / sglang_omni |
| `engine_family` | string | vllm / sglang |
| `weight` | float | 后端静态权重 |
| `healthy` | bool | 主动健康检查结果 |
| `inflight` | float | 网关侧在途请求数 |
| `running` | float | 引擎正在执行的请求数（直采归一化） |
| `waiting` | float | 引擎排队请求数 |
| `kv_usage` | float | KV cache 使用率（0~1） |
| `hit_rate` | float | 前缀缓存命中率（0~1，gauge 直读或 counter 对速率比推导） |
| `gen_tps` | float | 生成吞吐 token/s（gauge 直读或 counter 速率派生） |
| `prefix_match` | float | 网关前缀树对该后端的命中率（0~1） |
| `ttft_ewma` | float | 网关实测首字节时延的指数滑动均值（秒，α=0.2）；引擎无关的时延反馈闭环信号，尚无样本为 0 |
| `preempt_rate` | float | 引擎抢占速率（次/秒，由 `*:num_preemptions_total` 速率派生归一化）；PagedAttention 下抢占意味着整条请求 KV 被换出重算，是最强的过载负反馈，无该指标的引擎为 0 |
| `labels` | map[string]string | 后端自定义标签 |
| `raw` | map[string]float64 | 后端 /metrics 全部原始指标；counter 另有 `raw["rate:<指标名>"]` 速率派生值 |
| `vars` | map[string]float64 | 外部 Prometheus 注入变量（按 `prometheus.queries[].name` 索引，如 `vars["gpu_util"]`） |

expr 内置函数（`abs/ceil/floor/round/min/max` 等）可直接使用。
新增变量的流程：`scheduler.BuildEnv` 注册 + 本表更新 + 单测覆盖，三者缺一不可。

## 接口（实现见 engine.go）
```go
func NewEngine() *Engine
func (e *Engine) Set(name, filter, score string) error   // 编译并注册/热替换
func (e *Engine) Get(name string) *Policy
func (e *Engine) List() map[string]map[string]string     // 源码视图（admin 用）
func (p *Policy) Eval(env map[string]interface{}) (pass bool, score float64, err error)
```

## 内置方案预设（presets.go）
把推理调度的常见优化目标沉淀为可一键切换的表达式组合，供前端下拉选择：

| 预设 | 适用场景 | 核心取舍 |
|---|---|---|
| `balanced` | 通用基线 | 负载/KV/前缀命中均衡加权 |
| `cache_affinity` | 多轮对话、固定 system prompt 的 RAG | 前缀命中权重 ×2.5，最大化 prefix cache 复用 |
| `latency_first` | 交互式对话/补全 | 以 `ttft_ewma` 做反馈闭环，KV 水位收紧到 0.90、排队上限 32，牺牲容量换时延稳定 |
| `preemption_safe` | 长上下文、高压场景 | KV 水位收紧到 0.85 + `preempt_rate` 强惩罚 + KV 占用二次方惩罚，规避抢占重算 |
| `throughput_first` | 离线批量/异步任务 | 弱化运行数权重（连续批处理下批内并行近乎免费），向高 `gen_tps` 后端倾斜 |

预设只使用 Go 侧预计算的保证存在的变量（不依赖 `raw`/`vars` 的动态键），
任何引擎组合下都可安全求值。

使用方式：
- **配置引用**：`policies.<名>.preset: latency_first`（与手写 filter/score 二选一，加载期展开）；
- **运行期一键切换**：`POST /admin/presets/{name}/apply`（可选 `?policy=` 指定槽位，
  默认 `default`），复用策略热更通道——编译校验、持久化、集群广播全部生效；
- **前端渲染**：`GET /admin/presets` 返回方案清单（含中文标题与说明）与各策略槽位
  当前生效方案；生效方案由表达式反查得出（`MatchPreset`），手写表达式显示为 `custom`，
  无需额外存状态，集群各实例判定天然一致。

## 调试
`GET /admin/explain?model=&prompt=&policy=` 返回逐后端打分明细（升序），
回答"这条请求为什么会路由到 X"。

## 文件
| 文件 | 说明 |
|---|---|
| `engine.go` | 编译、注册、热替换与求值 |
| `presets.go` | 内置方案预设库与查找/反查 |
| `engine_test.go` / `presets_test.go` | 编译/求值/过滤/热更/异常路径、全预设可编译可求值用例 |
