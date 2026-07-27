# internal/backend/ — 后端抽象与注册表

## 职责
- `Backend`：单个推理后端实例的统一抽象，运行态（在途数、健康、隔离、熔断、
  指标快照、PromQL 变量）全部原子操作，调度热路径无锁读取；
- `Registry`：启动期由配置一次性构建的后端与模型路由注册表
  （含金丝雀分流子池 `Split`），运行期只读；
- 主动健康检查：周期 GET 各后端 `health_path`（默认 `/health`），2xx 视为健康，
  状态翻转触发回调（用于解绑粘性会话、唤醒排队请求）；
- 被动熔断：proxy 回写连续失败达到 `failure_threshold` 后进入 `eject_cooldown`
  冷却期，到期自动恢复（无需半开探测——主动健康检查兜底）。

## 引擎差异的处理位置
四种引擎（vllm / vllm_omni / sglang / sglang_omni）的差异集中在两处，
不设每引擎子包：
- **指标名映射**：`internal/metrics/adapters.go` 的按家族映射表
  （omni 变体经 `EngineType.Family()` 复用基础引擎表）；
- **PD 转发协议**：`internal/proxy/pdproxy.go` 按家族选择两段式（vllm）
  或 bootstrap 双发（sglang）。

## 核心接口（实现见各文件）
```go
func New(cfg config.BackendConfig) (*Backend, error)
func (b *Backend) Snapshot() *Snapshot            // 无锁读最近指标快照
func (b *Backend) TryAcquire() bool               // 并发额度（max_concurrency）
func (b *Backend) Available(now time.Time) bool   // 健康 && 未隔离 && 不在熔断冷却期
func (b *Backend) MarkFailure(threshold int32, cooldown time.Duration)
func (b *Backend) MarkSuccess()

func NewRegistry(cfg *config.Config) (*Registry, error)
func (r *Registry) Route(model string) (*Route, error)  // 未命中回退 "*" 兜底
func NewHealthChecker(backends []*Backend, interval, timeout time.Duration,
    onChange func(*Backend, bool)) *HealthChecker
```

## 文件
| 文件 | 说明 |
|---|---|
| `backend.go` | Backend 与全部运行态（含 Snapshot 定义） |
| `registry.go` | 注册表、模型路由与分流子池 |
| `health.go` | 主动健康检查循环 |
| `backend_test.go` | 并发额度、熔断/恢复、隔离用例 |
