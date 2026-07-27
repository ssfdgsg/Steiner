# internal/pd/ — PD 分离与 NCCL 链路管理

## 背景（为什么网关必须感知 NCCL 连接）
PD 分离（disaggregated serving）下，prefill 实例算完提示词后要把 KV cache 经
高速通道传给 decode 实例：

- **vLLM**：KVConnector 体系（PyNcclConnector / NixlConnector / Mooncake 等），
  请求经 `kv_transfer_params` 两段式协商；
- **SGLang**：`--disaggregation-mode prefill|decode`，请求携带
  `bootstrap_host/bootstrap_port/bootstrap_room` 三元组会合。

传输通道是**预建立、有成员关系**的：把请求派给未建链的 (P, D) 组合会直接失败
或退化为重算。因此配对必须收敛在已建链组合内。

## 职责与实现（manager = pd.go）
- **组模型**：`Group{Prefill 池, Decode 池, linksByPrefill}`；配置未声明
  `nccl_links` 时按全互联自动建链（默认带宽 100 Gbps）；
- **配对选路** `Group.Pick`：
  1. prefill 侧复用调度器 `PickAmong`（表达式/缓存感知等全部策略可用）；
  2. decode 侧只在与所选 prefill **有链路**的实例中选，
     打分 = decode 综合负载 + 链路拥塞度（在途传输数 / 带宽 × 系数），取最小；
- **链路在途计数**：转发期间 `Link.Acquire/Release`，暴露到
  `gateway_pd_link_inflight` 指标与 `GET /admin/pd`；
- 链路健康的进一步信号可经 PromQL 通道注入（如 RDMA 错误计数）由
  策略表达式消费。

## 边界
- 网关**不建立、不管理** NCCL 通道本身（那是推理引擎/传输层的职责），
  只维护其拓扑视图并施加调度约束；
- 两种转发协议的实现在 `internal/proxy/pdproxy.go`（按引擎家族自动选择）；
- 非 PD 路由完全旁路本包，零开销。

## 文件
`pd.go`（Manager/Group/Link 与配对选路）、`pd_test.go`
（全互联建链、链路约束、不健康跳过、空闲链路优先、在途计数用例）。
