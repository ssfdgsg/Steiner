# 最小化运行镜像：scratch 基底，只放一个静态二进制。
# 最终镜像 ≈ 二进制体积（~22MB），无 shell、无包管理器、无系统库，攻击面最小。
#
# 默认用法（自包含，宿主机不需要 Go）：
#   docker build -f deploy/docker/Dockerfile -t ai-gateway .
#
# 加速用法（境内）：
#   docker build -f deploy/docker/Dockerfile --build-arg GOPROXY=https://goproxy.cn,direct -t ai-gateway .
#
# 注：React 控制台产物（internal/webui/dist）已随仓库提交并由 go:embed 打进二进制，
# 镜像构建不需要 Node 阶段；改动前端后在开发机跑 `make web` 并提交 dist 即可。

FROM golang:1.22-alpine AS builder
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=${GOPROXY} GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux
WORKDIR /src

# 依赖层单独缓存：只有 go.mod/go.sum 变化时才重新下载。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# -s -w 去掉符号表与 DWARF 调试信息（约省 30%）；-trimpath 去掉本机路径，
# 同时让构建可复现。
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gateway ./cmd/gateway

# 运行阶段：scratch 零基底。
# ca-certificates 从构建镜像拷贝——外呼 HTTPS（webhook、OTLP、PromQL）需要根证书；
# 不装整个 alpine 只为这一个文件。
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/gateway /gateway
COPY configs/gateway.yaml /etc/ai-gateway/gateway.yaml

# scratch 无 /etc/passwd，直接以数值 UID 运行（非 root）。
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/gateway"]
CMD ["-config", "/etc/ai-gateway/gateway.yaml"]
