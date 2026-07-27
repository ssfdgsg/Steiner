# deploy/k8s/ — Kubernetes 清单

## 文件
| 文件 | 说明 |
|---|---|
| `gateway.yaml` | 单文件清单：ConfigMap（gateway.yaml 配置）+ Deployment（2 副本、`/healthz` 存活与就绪探针、prometheus.io 抓取注解、资源限额、非 root）+ ClusterIP Service |

## 使用
```bash
# 先用 deploy/docker/Dockerfile 构建并推送镜像，修改清单中 image 字段后：
kubectl apply -f deploy/k8s/gateway.yaml
kubectl port-forward svc/ai-gateway 8080:8080
```

## 要点
- 后端地址用 headless service 逐 Pod 直连（每个推理 Pod 是独立后端，
  负载均衡必须由网关做，不能让 kube-proxy 再均衡一层）；
- PD 分离部署时 prefill/decode 各建 StatefulSet，Pod 名稳定便于声明 `pd_groups`；
- 配置变更后滚动重启生效（策略表达式除外——可经 `PUT /admin/policies/{name}` 热更）；
- 使用 Prometheus Operator 时按集群约定自行补 ServiceMonitor。
