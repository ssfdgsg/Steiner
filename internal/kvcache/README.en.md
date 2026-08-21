# internal/kvcache/ — KV Cache Prefix-Aware Routing

> 🌐 English | [简体中文](README.md)

## Responsibility

Approximately replicates, on the gateway side, the prefix-cache content distribution of each backend: it maintains a compressed radix tree of “request prefix → set of backends that have served this prefix”, providing the per-backend prefix hit ratio (`prefix_match` variable and the cache_aware policy) to the scheduler. Requests carrying the same long prefix (multi-turn chats, RAG with a fixed system prompt) are routed back to the same backend as much as possible, hitting its RadixAttention / prefix cache and significantly reducing prefill cost and TTFT.

## Principles and Approximation

- No real tokenization (avoids per-model tokenizer dependencies): matching uses UTF-8 bytes as edge units in the compressed radix tree, consistent with sglang-router's character-level approximation — byte-prefix matching correlates highly with token-prefix matching; errors only affect the expected gain, not correctness;
- Match text = the concatenated request prompt (chat: role+content concatenated in order; multimodal requests use only text segments; see `internal/proxy/parse.go`); a single prefix is capped at `max_prefix_bytes` (default 4 KiB);
- The tree is an **optimistic approximation**: backends evict their own KV caches, so a high match ratio ≠ guaranteed hit; strict sync with backends is deliberately not pursued.

## Implementation (tree = radix.go)

- Nodes carry `owners` (backend ID → last access time); Insert marks ownership along the path; a single Match descent returns the **per-backend** longest matching byte count;
- Eviction: TTL expiry (default 10m) + periodic pruning (`RunPruner`, default 30s); empty leaf nodes are reclaimed; size statistics (bytes / node count) are exposed in self metrics and `GET /admin/kvcache`;
- **Hard memory cap (H8)**: `NewTreeWithBudget` enforces a dual-dimension budget — node count (`max_nodes`, default 100k) and edge bytes (`max_bytes`, default 256MiB); on overflow `Insert` evicts by “oldest-accessed owner” generations (reusing TTL-prune semantics, dropping the oldest generation per pass) and reclaims empty nodes, keeping tree size bounded under abnormal/high-cardinality input; `NewTree` (no budget) stays backward-compatible, and a budget of 0 means unlimited for that dimension;
- **Owner cleanup on backend removal (L4)**: `RemoveBackedBy(backendID)` purges all prefixes owned by a removed backend (called via the registry removal hook), so a re-registered backend with the same ID never inherits stale affinity to an instance that does not yet hold the KV cache, and stale owners no longer occupy memory until TTL expiry;
- Concurrency: a single mutex — tree operations are microsecond-scale and tree size is bounded by the TTL and the prefix cap; measured results show no sharding needed. If it ever becomes a bottleneck, shard by root first byte into 256 partitions (naturally independent).

## In-House Implementation Note (documented per the CLAUDE.md architecture-priority requirement)

`armon/go-radix` and `hashicorp/go-immutable-radix` were both evaluated: they are general-purpose KV structures and do not support “multi-owner nodes + per-owner timestamps + per-backend match length in a single descent”; the retrofit cost outweighs the benefit, so a dedicated tree was implemented as a documented special case (structure modeled after sglang-router's tree).

## Files

`radix.go` (tree + TTL pruning + stats), `radix_test.go` (split / match / multi-owner / truncation / expiry cases).
