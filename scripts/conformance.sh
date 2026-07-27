#!/usr/bin/env bash
# 真实引擎 conformance 验证编排：
#   1. docker compose 启动官方 vLLM CPU 镜像 + 小模型（首次运行会下载模型，耗时较长）；
#   2. 拉起网关（configs/conformance.yaml，指向真实引擎）；
#   3. 运行 test/conformance 验收测试（指标名适配 / SSE / usage / 改写 / 健康）；
#   4. 清理（KEEP_UP=1 保留环境便于反复调试；模型缓存卷始终保留）。
#
# 可调环境变量：
#   VLLM_IMAGE          引擎镜像（默认 vllm/vllm-openai-cpu:latest-x86_64）
#   CONFORMANCE_MODEL   模型（默认 HuggingFaceTB/SmolLM2-135M-Instruct，约 270MB）
#   HF_ENDPOINT         HuggingFace 源（默认 https://hf-mirror.com，出海网络可置空）
#   ENGINE_READY_TIMEOUT 引擎就绪等待秒数（默认 900，含首次模型下载）
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f deploy/conformance/docker-compose.yaml"
GATEWAY_PORT=18190
VLLM_PORT=18100
ENGINE_READY_TIMEOUT="${ENGINE_READY_TIMEOUT:-900}"
GW_PID=""

cleanup() {
  [[ -n "$GW_PID" ]] && kill "$GW_PID" 2>/dev/null || true
  if [[ "${KEEP_UP:-0}" != "1" ]]; then
    $COMPOSE down --timeout 10 >/dev/null 2>&1 || true
  else
    echo "==> KEEP_UP=1，保留 vLLM 容器（手动清理: $COMPOSE down）"
  fi
}
trap cleanup EXIT

# 缺省模型走宿主预下载 + 只读挂载：容器内 HF 直连间歇性失败（WSL2 常见），
# 宿主 curl 带重试更稳，且下载一次后离线可复跑。
MODEL_DIR="deploy/conformance/model-cache/smollm2"
if [[ -z "${CONFORMANCE_MODEL:-}" && ! -f "$MODEL_DIR/model.safetensors" ]]; then
  HF_BASE="${HF_ENDPOINT:-https://hf-mirror.com}/HuggingFaceTB/SmolLM2-135M-Instruct/resolve/main"
  echo "==> 预下载模型到宿主（$HF_BASE，约 270MB）"
  mkdir -p "$MODEL_DIR"
  for f in config.json generation_config.json model.safetensors tokenizer.json \
           tokenizer_config.json special_tokens_map.json vocab.json merges.txt; do
    curl -fSL --retry 5 --retry-delay 3 --connect-timeout 15 \
      -o "$MODEL_DIR/$f.part" "$HF_BASE/$f" && mv "$MODEL_DIR/$f.part" "$MODEL_DIR/$f" \
      || { rm -f "$MODEL_DIR/$f.part"; echo "    (跳过可选文件 $f)"; }
  done
  [[ -f "$MODEL_DIR/model.safetensors" && -f "$MODEL_DIR/config.json" ]] || {
    echo "!! 模型权重下载失败，请检查网络或改用 HF_ENDPOINT= 官方源重试" >&2
    exit 1
  }
fi

echo "==> 启动真实 vLLM（CPU）"
$COMPOSE up -d

echo "==> 等待引擎就绪（上限 ${ENGINE_READY_TIMEOUT}s，首次运行含模型下载）"
for ((i = 0; i < ENGINE_READY_TIMEOUT; i += 5)); do
  if curl -sf "http://127.0.0.1:${VLLM_PORT}/health" >/dev/null 2>&1; then
    echo "    引擎就绪（${i}s）"
    break
  fi
  # 容器带 on-failure 重启策略：running/restarting 都算存活，只有彻底
  # 停止（重启次数耗尽）才判失败。
  cid="$($COMPOSE ps -q vllm 2>/dev/null || true)"
  state="$(docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null || echo missing)"
  if [[ "$state" == "exited" || "$state" == "dead" || "$state" == "missing" ]]; then
    echo "!! 引擎容器已退出（state=$state），最近日志：" >&2
    $COMPOSE logs --tail 30 vllm >&2
    exit 1
  fi
  sleep 5
done
curl -sf "http://127.0.0.1:${VLLM_PORT}/health" >/dev/null || {
  echo "!! 引擎在 ${ENGINE_READY_TIMEOUT}s 内未就绪，最近日志：" >&2
  $COMPOSE logs --tail 30 vllm >&2
  exit 1
}

echo "==> 构建并启动网关（:${GATEWAY_PORT}）"
go build -o /tmp/gateway-conformance ./cmd/gateway
/tmp/gateway-conformance -config configs/conformance.yaml >/tmp/gateway-conformance.log 2>&1 &
GW_PID=$!
for ((i = 0; i < 30; i++)); do
  curl -sf "http://127.0.0.1:${GATEWAY_PORT}/healthz" >/dev/null 2>&1 && break
  kill -0 "$GW_PID" 2>/dev/null || { echo "!! 网关启动失败：" >&2; cat /tmp/gateway-conformance.log >&2; exit 1; }
  sleep 1
done

echo "==> 运行 conformance 验收测试"
GATEWAY_CONFORMANCE=1 \
  CONFORMANCE_GATEWAY="http://127.0.0.1:${GATEWAY_PORT}" \
  CONFORMANCE_VLLM="http://127.0.0.1:${VLLM_PORT}" \
  go test ./test/conformance -count=1 -v

echo "✅ conformance 验证全部通过"
