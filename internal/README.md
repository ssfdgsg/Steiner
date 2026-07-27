# internal/ — 网关核心实现

所有业务模块均在此，禁止被外部项目导入（Go internal 语义）。
模块具体装配发生在 `cmd/gateway/main.go`。

## 模块依赖关系（自上而下单向依赖，无环）

```
server ─▶ proxy ─▶ scheduler ─▶ policy（表达式求值）
   │        │          └──────▶ kvcache（前缀命中率）
   │        ├─▶ pd（PD 配对 + NCCL 链路）─▶ scheduler
   │        ├─▶ session（会话粘性）
   │        └─▶ queue（容量排队）
   ├─▶ alerting（告警/扩缩容，读 backend 快照）
   └─▶ metrics（自身指标 Registry）

backend（注册表/健康/熔断）◀── metrics.Scraper / PromCollector（写入快照）
config（被所有模块引用，自身零内部依赖）    pkg/openai（协议类型）
```

## 各模块一览
| 模块 | 一句话职责 |
|---|---|
| `config` | 配置加载、默认值、静态校验 |
| `server` | HTTP 接入、路由注册、admin API、优雅退出 |
| `proxy` | 转发执行：限流、粘性、排队、SSE 透传、重试、PD 协议、模型名改写 |
| `backend` | 后端抽象、注册表（含分流子池）、健康检查、被动熔断 |
| `metrics` | 直采 + PromQL 双通道 → 统一快照；自身指标导出 |
| `policy` | 策略表达式动态编译与热更新 |
| `scheduler` | 八种调度策略与打分解释 |
| `kvcache` | 前缀感知（radix tree）亲和路由 |
| `pd` | PD 分离组与 NCCL 链路拓扑/在途计数 |
| `session` | 会话粘性存储 |
| `queue` | 容量排队与唤醒 |
| `alerting` | webhook 告警规则引擎与自动扩缩容建议器 |

## 全局约定
- 每个包对外暴露小接口 + `New*` 构造函数；
- 后台循环统一接受 `context.Context`，由 main 统一取消；
- 所有外呼（后端探测、抓取、PromQL、webhook）必须带超时；
- 后端快照与 PromQL 变量用原子指针发布，调度热路径**无锁读**。
