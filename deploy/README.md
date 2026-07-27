# deploy/ — 部署物

## 子目录
| 目录 | 内容 |
|---|---|
| `docker/` | `Dockerfile`（多阶段构建，含 gateway 与 mockbackend 两个二进制）、`docker-compose.yaml`（gateway + 2×mock 后端 + Prometheus 本地全家桶）、`gateway-compose.yaml`（编排内网关配置）、`prometheus.yml` |
| `k8s/` | `gateway.yaml`：ConfigMap + Deployment（探针/资源/抓取注解）+ Service 单文件清单 |
| `grafana/` | `dashboard.json`：QPS/错误率/TTFT 分位/后端负载与健康/重试限流/前缀树与队列/PD 链路/生成吞吐/告警与扩缩容 九面板看板 |

## 部署要点
- 网关无状态（会话粘性与前缀树为内存态）：多副本部署时需前置 LB 按会话键
  一致性哈希，否则粘性与前缀亲和收益跨副本稀释；
- `/admin/*`（含 `/admin/ui/` 控制台）与 `/metrics` 与数据面同端口：
  暴露公网时务必在入口层（Ingress/LB）挡掉 `/admin` 前缀，或加认证；
  控制台无内置鉴权——设计上假定它只在受信网络或反代认证之后可达；
- 资源基线：单副本 2C/512Mi 起步（前缀树受 TTL 与前缀上限约束，
  典型占用几十 MiB 量级）；
- K8s 内推荐用 headless service 逐 Pod 直连（每个推理 Pod 是独立后端，
  负载均衡必须由网关做，不能让 kube-proxy 再均衡一层）；PD 分离部署时
  prefill/decode 各建 StatefulSet，Pod 名稳定便于声明 `pd_groups`。
