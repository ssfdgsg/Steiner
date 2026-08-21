# internal/pd/ — PD Disaggregation and NCCL Link Management

> 🌐 English | [简体中文](README.md)

## Background (Why the Gateway Must Be Aware of NCCL Connections)

Under PD disaggregation (disaggregated serving), a prefill instance must transfer its KV cache to a decode instance over a high-speed channel:

- **vLLM**: the KVConnector family (PyNcclConnector / NixlConnector / Mooncake, etc.); requests negotiate in two phases via `kv_transfer_params`;
- **SGLang**: `--disaggregation-mode prefill|decode`; requests rendezvous with a `bootstrap_host/bootstrap_port/bootstrap_room` triple.

Transfer channels are **pre-established and have membership**: dispatching a request to a (P, D) pair with no established link fails outright or degrades to recompute. Pairing must therefore converge within already-linked combinations.

## Responsibility and Implementation (manager = pd.go)

- **Group model**: `Group{Prefill pool, Decode pool, linksByPrefill}`; when `nccl_links` is not declared in config, links are auto-created as a full mesh (default bandwidth 100 Gbps);
- **Pair routing** `Group.Pick`:
  1. The prefill side reuses the scheduler's `PickAmong` (all policies available, including expression and cache-aware);
  2. The decode side selects only among instances **linked** to the chosen prefill, scoring = decode composite load + link congestion (inflight transfers / bandwidth × coefficient), taking the minimum;
- **Link inflight counting**: `Link.Acquire/Release` during forwarding, exposed via the `gateway_pd_link_inflight` metric and `GET /admin/pd`;
- Further link-health signals can be injected through the PromQL channel (e.g. RDMA error counts) and consumed by policy expressions.

## Boundaries

- The gateway **does not establish or manage** NCCL channels themselves (that is the inference engine / transport layer's job); it only maintains a topology view and imposes scheduling constraints;
- The two forwarding protocols are implemented in `internal/proxy/pdproxy.go` (auto-selected by engine family);
- Non-PD routing bypasses this package entirely — zero overhead.

## Files

`pd.go` (Manager/Group/Link and pair routing), `pd_test.go` (full-mesh link creation, link constraints, unhealthy-skip, idle-link preference, inflight counting cases).
