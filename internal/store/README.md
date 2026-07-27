# internal/store — 动态配置持久层

admin 运行期变更落库，重启自动恢复。`store.enabled=false`（缺省）时行为与
无持久层完全一致。支持 **PostgreSQL** 与 **MySQL**，方言差异（`$n`/`?` 占位符、
`ON CONFLICT`/`ON DUPLICATE KEY` upsert）集中在 `dialect`，DDL 共用两家交集
类型（JSON 数据存 TEXT）。

## 持久化内容与语义

| 表 | 内容 | 写入时机 |
|---|---|---|
| `gateway_backends` | 动态注册的后端 + 挂载的模型路由 | `POST /admin/backends`（失败回滚注册表）/ `DELETE` |
| `gateway_policies` | 热更新过的策略表达式 | `PUT /admin/policies/{name}` |

**启动合并**：YAML 是初始基线，DB 是运行期变更的权威来源——启动时
`ListBackends`/`ListPolicies` 逐条应用（Upsert 语义），同名项覆盖 YAML；
引用已不存在路由的 DB 行跳过并告警，不阻塞启动。

**集群配合**：变更的持久化只由发起实例执行一次，其余实例经 Redis 广播
只更新内存态，避免多实例重复写库。

## 验证

- `store_test.go`：sqlmock 双方言验证 SQL 分支与参数绑定（随 `make test` 运行）；
- `store_live_test.go`：真实数据库全链路（建表/upsert/加载/删除），
  设置 `GATEWAY_TEST_PG_DSN` / `GATEWAY_TEST_MYSQL_DSN` 后启用，默认跳过。
