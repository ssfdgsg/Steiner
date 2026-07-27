# deploy/docker/ — 容器化与本地编排

## 文件
| 文件 | 说明 |
|---|---|
| `Dockerfile` | 多阶段构建：`golang:1.22-alpine` 编译（CGO 关闭、strip）→ `alpine:3.20` 非 root 运行；镜像同时包含 `gateway` 与 `mockbackend` 两个二进制（后者供 compose 演示用） |
| `docker-compose.yaml` | 本地全家桶：`gateway` + `mock-vllm` + `mock-sglang` + `prometheus`，开箱可跑全部数据面/管理面场景 |
| `gateway-compose.yaml` | 编排内网关配置（后端指向 mock 服务名） |
| `prometheus.yml` | Prometheus 抓取配置：抓网关 `/metrics`（含后端归一化视图）与两个 mock 原生指标 |

## 使用
```bash
docker compose -f deploy/docker/docker-compose.yaml up --build -d
curl -s localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-model","messages":[{"role":"user","content":"你好"}]}'
curl -s localhost:8080/admin/backends   # 管理面
# Prometheus: localhost:9090
```

境内环境构建加速：`docker compose build --build-arg GOPROXY=https://goproxy.cn,direct`。
