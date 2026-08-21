# 使用 GVM 当前选择的本地 Go 工具链，避免自动下载其他版本导致卡死
export GOTOOLCHAIN := local

.PHONY: build test stress vet fmt smoke tidy bench conformance web web-dev web-install \
        run image image-selfcontained up down

# 本地直接启动（读 configs/gateway.yaml）。控制台：http://localhost:8080/admin/ui/
run: build
	go run ./cmd/gateway -config configs/gateway.yaml

# 最小化镜像：宿主机编译 → 搬进 scratch（推荐；构建快，压缩传输 ~8MB）
image:
	bash scripts/build-image.sh

# 自包含镜像：容器内编译（宿主机无需 Go），最终同为 scratch 基底
image-selfcontained:
	docker build -f deploy/docker/Dockerfile -t ai-gateway:slim .

# 本地全家桶：网关 + 2×mock 后端 + Prometheus
up:
	docker compose -f deploy/docker/docker-compose.yaml up --build -d
	@echo "控制台: http://localhost:8080/admin/ui/   Prometheus: http://localhost:9090"

down:
	docker compose -f deploy/docker/docker-compose.yaml down

build:
	go build ./...

# React 控制台构建：产物输出到 internal/webui/dist，由 go:embed 打进二进制。
# dist/ 已提交仓库，因此 `make build` 不需要 Node——只有改前端时才跑本目标。
web:
	cd web && npm install --no-audit --no-fund && npm run build

web-install:
	cd web && npm install --no-audit --no-fund

# 前端热更开发：Vite dev server（:5173）代理 /admin 与 /v1 到本地网关 :8080。
# 另开一个终端跑网关：go run ./cmd/gateway -config configs/gateway.yaml
web-dev:
	cd web && npm run dev

test:
	go test ./... -count=1

# 压力回归：管理面并发指标写入/聚合、历史缓冲读写和端点轮询启用竞态检测，
# 数据面验证大 query / response 完整性，随后采集管理面容量基准。
stress:
	go test -race ./internal/metrics ./internal/server \
		-run 'TestAggregateConcurrentStress|TestHistoryConcurrentReadWriteStress|TestStatsEndpointsConcurrentStress' \
		-count=3
	go test ./internal/proxy \
		-run 'TestLargeRequestBodyAndQueryForward|TestLargeNonStreamingResponse|TestLargeStreamingResponse' \
		-count=1
	go test ./internal/proxy -run '^$$' \
		-bench 'BenchmarkParseLargeRequest|BenchmarkLargeRequestForward16MiB' \
		-benchmem -benchtime=3x -count=1
	go test ./internal/metrics -run '^$$' \
		-bench 'BenchmarkAggregateHighCardinality|BenchmarkHistoryFullBufferSample' \
		-benchmem -benchtime=2s -count=1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

# 冒烟验证：启动 mock 后端与网关，发送一次补全请求（脚本见 scripts/）
smoke: build
	bash scripts/smoke.sh

# 性能基线：调度选路 / 前缀树 / 转发链路（基线数据见 docs/perf-baseline.md）
bench:
	go test -run '^$$' -bench . -benchmem -count=1 ./internal/scheduler ./internal/kvcache ./internal/proxy

# 真实引擎验证：docker 启动官方 vLLM CPU 镜像 + 小模型，验证协议边界
# （指标名适配 / SSE 分帧 / usage 提取 / 模型名改写 / 健康检查）
conformance:
	bash scripts/conformance.sh
