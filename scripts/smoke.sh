#!/usr/bin/env bash
# 本地冒烟验证：启动两个 mock 后端（vllm + sglang）与网关，
# 依次验证健康探针、模型清单、非流式补全、SSE 流式补全、管理面与自身指标。
# 用法：bash scripts/smoke.sh（或 make smoke）
set -euo pipefail

export GOTOOLCHAIN=local
cd "$(dirname "$0")/.."

VLLM_PORT=18001
SGLANG_PORT=18002
GW_PORT=18080
WORKDIR="$(mktemp -d)"
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "==> 构建"
go build -o "$WORKDIR/gateway" ./cmd/gateway
go build -o "$WORKDIR/mockbackend" ./test/mockbackend/cmd

echo "==> 启动 mock 后端 (vllm:$VLLM_PORT, sglang:$SGLANG_PORT)"
"$WORKDIR/mockbackend" -type vllm -port "$VLLM_PORT" &
PIDS+=($!)
"$WORKDIR/mockbackend" -type sglang -port "$SGLANG_PORT" &
PIDS+=($!)

cat > "$WORKDIR/gateway.yaml" <<EOF
server:
  listen: ":$GW_PORT"
metrics:
  scrape_interval: 1s
kv_cache:
  enabled: true
session:
  enabled: true
queue:
  enabled: true
backends:
  - { id: vllm-a,   engine: vllm,   url: "http://127.0.0.1:$VLLM_PORT" }
  - { id: sglang-a, engine: sglang, url: "http://127.0.0.1:$SGLANG_PORT" }
models:
  - name: smoke-model
    backends: [vllm-a, sglang-a]
    strategy: expression
  - name: "*"
    backends: [vllm-a, sglang-a]
    strategy: cache_aware
EOF

echo "==> 启动网关 (:$GW_PORT)"
"$WORKDIR/gateway" -config "$WORKDIR/gateway.yaml" &
PIDS+=($!)

wait_http() {
  local url=$1 name=$2
  for _ in $(seq 1 50); do
    if curl -sf -o /dev/null "$url"; then return 0; fi
    sleep 0.1
  done
  echo "!! $name 启动超时: $url" >&2
  exit 1
}
wait_http "http://127.0.0.1:$VLLM_PORT/health" "vllm mock"
wait_http "http://127.0.0.1:$SGLANG_PORT/health" "sglang mock"
wait_http "http://127.0.0.1:$GW_PORT/healthz" "网关"

fail() { echo "!! 冒烟失败: $1" >&2; exit 1; }

echo "==> 1/6 模型清单"
curl -sf "http://127.0.0.1:$GW_PORT/v1/models" | grep -q "smoke-model" || fail "/v1/models 缺少模型"

echo "==> 2/6 非流式补全"
RESP=$(curl -sf -X POST "http://127.0.0.1:$GW_PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"smoke-model","messages":[{"role":"user","content":"你好"}]}')
echo "$RESP" | grep -q "模拟应答" || fail "非流式补全响应异常: $RESP"

echo "==> 3/6 SSE 流式补全"
STREAM=$(curl -sf -N -X POST "http://127.0.0.1:$GW_PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"smoke-model","stream":true,"messages":[{"role":"user","content":"你好"}]}')
echo "$STREAM" | grep -q "data:" || fail "SSE 流缺少 data 帧"
echo "$STREAM" | grep -q "\[DONE\]" || fail "SSE 流缺少 [DONE]"

echo "==> 4/6 管理面：后端清单与打分解释"
curl -sf "http://127.0.0.1:$GW_PORT/admin/backends" | grep -q "vllm-a" || fail "/admin/backends 异常"
curl -sf "http://127.0.0.1:$GW_PORT/admin/explain?model=smoke-model&prompt=hello" \
  | grep -q "scores" || fail "/admin/explain 异常"

echo "==> 5/6 策略热更新"
curl -sf -X PUT "http://127.0.0.1:$GW_PORT/admin/policies/default" \
  -H 'Content-Type: application/json' \
  -d '{"filter":"healthy","score":"waiting * 9.0"}' | grep -q updated || fail "策略热更新失败"

echo "==> 6/6 自身指标"
sleep 1.2  # 等待一轮指标抓取
METRICS=$(curl -sf "http://127.0.0.1:$GW_PORT/metrics")
echo "$METRICS" | grep -q "gateway_requests_total" || fail "缺少 gateway_requests_total"
# 注：gateway_backend_inflight 等 GaugeVec 由 5s 周期刷新，冒烟窗口内可能尚无样本，
# 这里断言常驻的普通 Gauge 与 TTFT 直方图。
echo "$METRICS" | grep -q "gateway_kvtree_bytes" || fail "缺少 gateway_kvtree_bytes"
echo "$METRICS" | grep -q "gateway_time_to_first_byte_seconds" || fail "缺少 TTFT 直方图"

echo "✅ 冒烟验证全部通过"
