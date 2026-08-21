# internal/proxy/ — 转发执行层

> 🌐 [English](README.en.md) | 简体中文

## 职责
OpenAI 兼容 API 的反向代理：请求解析 → 限流 → 会话粘性/排队 → 调度 → 转发。

- **请求解析**（parse.go）：宽松 map 解析，提取 model / stream / 提示词文本
  （多模态 content 数组取 text 分段）/ 会话键（X-Session-Id 头 > body.user）/ priority；
  非 JSON 不报错，只影响可用的调度信号；
- **限流**：模型级令牌桶（`golang.org/x/time/rate`，`rate_limit_qps/burst`），超限 429；
- **会话粘性**：绑定后端仍可用则短路选路；成功应答后（重新）绑定；
- **容量排队**：选路无可用容量且启用 queue 时挂起等待容量释放（请求完成、
  后端恢复健康都会广播信号），队列满 429 / 等待超时 503；
- **模型名改写**：路由配置 `rewrite_model` 时改写请求体 model 字段后转发；
- **流式透传**（streamResponse）：手写转发而非 `httputil.ReverseProxy`——
  需要"重试换后端 + PD 两段式 + TTFT 打点"，单目标的 ReverseProxy 不满足。
  逐块读写并立即 Flush（SSE 关键），首字节打 TTFT 点，流尾提取 usage 记录 token 用量；
- **重试语义**：仅在未向客户端写出任何字节前重试（连接失败 / 上游 502/503/504），
  换后端并计入被动熔断；4xx 是请求本身的问题，原样透传不重试；
  首字节已写出后绝不重试，只能断流并记录。

## PD 分离转发（pdproxy.go）
按 prefill 实例引擎家族自动选择协议：

- **vllm 家族**（NIXL/NCCL KVConnector 两段式）：
  1. prefill 请求：`max_tokens=1`、关流式、注入 `kv_transfer_params{do_remote_decode:true}`；
  2. decode 请求：注入 prefill 响应回传的 `kv_transfer_params`，响应流式回传客户端；
- **sglang 家族**（bootstrap 三元组并发双发）：prefill 与 decode 同时收到携带相同
  `bootstrap_host / bootstrap_port / bootstrap_room` 的请求完成会合；
  客户端响应取自 decode 流，prefill 响应后台校验。

链路（NCCL）在途传输计数在配对期间 Acquire/Release，暴露到
`gateway_pd_link_inflight` 与 `GET /admin/pd`。
prefill 失败换 prefill 重试（受 `server.retries` 约束）。

## 文件
| 文件 | 说明 |
|---|---|
| `proxy.go` | 入口、限流、粘性、排队、转发与重试、流式透传 |
| `parse.go` | 请求特征提取 |
| `pdproxy.go` | PD 两段式 / 双发协议 |
| `proxy_test.go` / `pd_test.go` / `feature_test.go` / `route_test.go` | 转发、流式、重试、PD、粘性、排队、分流用例 |
