# internal/proxy/ — Forwarding Execution Layer

> 🌐 English | [简体中文](README.md)

## Responsibility

Reverse proxy for the OpenAI-compatible API: request parsing → rate limiting → session stickiness / queueing → scheduling → forwarding.

- **Request parsing** (parse.go): lenient map parsing, extracting model / stream / prompt text (multimodal content arrays take text segments) / session key (X-Session-Id header > body.user) / priority; non-JSON bodies do not error, they only reduce the scheduling signals available;
- **Rate limiting**: model-level token bucket (`golang.org/x/time/rate`, `rate_limit_qps/burst`), 429 on overflow;
- **Session stickiness**: short-circuit routing when the bound backend is still available; (re)bind after a successful response;
- **Capacity queueing**: when no candidate has capacity and queueing is enabled, suspend and wait for capacity to be released (request completion and backend recovery both broadcast signals); 429 when the queue is full, 503 on wait timeout;
- **Model-name rewriting**: when the route config sets `rewrite_model`, rewrite the body's `model` field before forwarding;
- **Streaming passthrough** (streamResponse): hand-written forwarding instead of `httputil.ReverseProxy` — the requirements “retry on another backend + PD two-phase + TTFT instrumentation” exceed a single-target ReverseProxy. Reads/writes chunk-by-chunk with an immediate Flush (critical for SSE), stamps TTFT at the first byte, and extracts `usage` at stream end to record token counts;
- **Retry semantics**: retries happen only before any byte has been written to the client (connection failure / upstream 502/503/504), switching backends and counting toward passive circuit breaking; 4xx is a request-side problem and passes through verbatim without retry; once the first byte is written, never retry — only truncate the stream and record it.

## PD Disaggregation Forwarding (pdproxy.go)

Auto-selects the protocol by the prefill instance's engine family:

- **vllm family** (NIXL/NCCL KVConnector two-phase):
  1. Prefill request: `max_tokens=1`, streaming off, inject `kv_transfer_params{do_remote_decode:true}`;
  2. Decode request: inject the `kv_transfer_params` returned by the prefill response; stream the response back to the client;
- **sglang family** (bootstrap triple, concurrent dual-send): prefill and decode both receive requests carrying the same `bootstrap_host / bootstrap_port / bootstrap_room` to rendezvous; the client response comes from the decode stream; the prefill response is validated in the background.

Link (NCCL) inflight transfer counts are Acquire/Release'd during pairing and exposed via `gateway_pd_link_inflight` and `GET /admin/pd`.
Prefill failures retry on another prefill (bounded by `server.retries`).

## Files

| File | Description |
|---|---|
| `proxy.go` | Entry, rate limiting, stickiness, queueing, forwarding and retry, streaming passthrough |
| `parse.go` | Request feature extraction |
| `pdproxy.go` | PD two-phase / dual-send protocols |
| `proxy_test.go` / `pd_test.go` / `feature_test.go` / `route_test.go` | Forwarding, streaming, retry, PD, stickiness, queueing, split cases |
