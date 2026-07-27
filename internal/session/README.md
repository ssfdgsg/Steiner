# internal/session/ — 会话粘性

## 职责
同一会话的多轮请求固定路由到同一后端，与 `kvcache` 前缀亲和互补：
粘性是**确定性** O(1) 查表短路，前缀匹配是**概率性**收益；
多轮对话两者叠加可获得最高的后端 KV cache 命中率。

## 行为定义（wiring 见 internal/proxy 与 cmd/gateway）
- 会话键：请求头 `X-Session-Id` 优先，缺省回退请求体 `user` 字段；无键请求不粘；
- 命中粘性但目标后端不可用（不健康/隔离/熔断/不在路由池）→ 放弃粘性走正常调度，
  成功后**改绑**新后端；
- TTL 滑动续期（默认 10m）；容量上限（默认 10 万）超限按最久未用淘汰；
- 后端健康检查发现下线 → `InvalidateBackend` 批量解绑（其 KV cache 已失效）。

## 实现
64 分片 map + 滑动过期时间戳；近似 LRU（滑动续期使活跃会话到期时间靠后，
淘汰最早到期项即最久未活跃项）。单机内存态；网关多副本部署时需由前置 LB
按会话键做一致性哈希（跨副本共享粘性表需外部存储，暂不引入——保持零依赖）。

## 接口
```go
func NewStore(ttl time.Duration, maxEntries int) *Store
func (s *Store) Lookup(key string) (backendID string, ok bool) // 命中滑动续期
func (s *Store) Bind(key, backendID string)
func (s *Store) InvalidateBackend(backendID string)
```

## 文件
`store.go`、`store_test.go`（绑定/续期/过期/失效/容量淘汰用例）。
