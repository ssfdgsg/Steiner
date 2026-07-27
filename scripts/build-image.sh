#!/usr/bin/env bash
# 构建最小化容器镜像：宿主机编译静态二进制 → 搬进 scratch 镜像。
#
# 相比在容器内编译（deploy/docker/Dockerfile），本方式：
#   - 构建快（复用宿主机 Go 构建缓存，秒级）；
#   - 上下文小（只传二进制与配置，不传源码）；
#   - 最终镜像体积相同（都是 scratch + 二进制）。
#
# 用法：
#   bash scripts/build-image.sh                 # 默认 tag ai-gateway:slim
#   TAG=registry/ai-gateway:v1 bash scripts/build-image.sh
set -euo pipefail

export GOTOOLCHAIN=local
cd "$(dirname "$0")/.."

TAG="${TAG:-ai-gateway:slim}"
VERSION="${VERSION:-dev}"

echo "==> 编译静态二进制（CGO_ENABLED=0，strip 符号）"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH:-amd64}" \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o dist/gateway ./cmd/gateway

# 静态链接校验：scratch 镜像里没有动态链接器，一旦动态链接会启动即失败。
if command -v file >/dev/null 2>&1; then
  if file dist/gateway | grep -q "dynamically linked"; then
    echo "!! 二进制是动态链接，scratch 镜像无法运行；请确认 CGO_ENABLED=0" >&2
    exit 1
  fi
fi
echo "    产物大小: $(du -h dist/gateway | cut -f1)"

echo "==> 准备根证书（外呼 HTTPS：webhook / OTLP / PromQL 需要）"
for c in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
  if [ -f "$c" ]; then cp "$c" dist/ca-certificates.crt; break; fi
done
if [ ! -f dist/ca-certificates.crt ]; then
  echo "!! 未找到宿主机根证书，从 alpine 镜像提取"
  docker run --rm alpine:3.20 cat /etc/ssl/certs/ca-certificates.crt > dist/ca-certificates.crt
fi

echo "==> 构建镜像 $TAG"
docker build -f deploy/docker/Dockerfile.prebuilt -t "$TAG" .

echo "==> 完成"
docker images "$TAG" --format '    {{.Repository}}:{{.Tag}}  {{.Size}}'
echo
echo "运行：docker run --rm -p 8080:8080 -v \$PWD/configs/gateway.yaml:/etc/ai-gateway/gateway.yaml:ro $TAG"
