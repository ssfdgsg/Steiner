# test/mockbackend/ — 四引擎模拟器

## 职责
可执行/可内嵌的 HTTP 服务，模拟 vLLM / vLLM-Omni / SGLang / SGLang-Omni
的对外行为，用于集成测试与本地冒烟，不依赖 GPU。

## 模拟能力
- `POST /v1/chat/completions`、`POST /v1/completions`：JSON 应答（含 usage）；
  `stream=true` 时输出 SSE 流（三块内容 + 末块 usage + `[DONE]`）；
- `GET /metrics`：按引擎家族渲染指标文本（`vllm:` / `sglang:` 前缀），
  数值来自可编程状态；
- `GET /health`：可编程返回 200/503；
- 故障注入（HTTP 或进程内 `SetState/SetFault`）：
  - `POST /mock/state`：设置 running/waiting/kv_usage/cache_hit_rate/gen_tokens 与健康状态；
  - `POST /mock/fault`：注入固定错误码、首字节延迟（TTFT）、SSE 流中断；
- **PD 行为模拟**：vllm 家族请求携带 `kv_transfer_params{do_remote_decode:true}`
  时按 prefill 语义返回 KV 传输句柄；`LastBody()` 记录最近一次请求体，
  供用例断言 bootstrap / kv_transfer_params 注入。

## 使用方式
```go
// 内嵌（集成测试）
mb := mockbackend.New("vllm")           // vllm | vllm_omni | sglang | sglang_omni
srv := httptest.NewServer(mb.Handler())
```
```bash
# 独立进程（冒烟脚本）
go run ./test/mockbackend/cmd -type sglang -port 30001
```

## 文件
`mock.go`（状态机 + 全部 handler + 指标渲染 + SSE）、`cmd/main.go`（独立进程入口）。
