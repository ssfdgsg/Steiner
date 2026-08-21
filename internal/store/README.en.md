# internal/store — Dynamic Configuration Persistence

> 🌐 English | [简体中文](README.md)

Admin runtime changes are persisted to the database and restored automatically on restart. With `store.enabled=false` (the default) behavior is identical to having no persistence layer. Supports **PostgreSQL** and **MySQL**; dialect differences (`$n`/`?` placeholders, `ON CONFLICT`/`ON DUPLICATE KEY` upsert) are concentrated in `dialect`, and DDL uses the intersection of the two (JSON data is stored as TEXT).

## Persisted Content and Semantics

| Table | Content | Write timing |
|---|---|---|
| `gateway_backends` | Dynamically registered backends + mounted model routes | `POST /admin/backends` (rolls back the registry on failure) / `DELETE` |
| `gateway_policies` | Hot-reloaded policy expressions | `PUT /admin/policies/{name}` |

**Startup merge**: YAML is the initial baseline; the DB is the authoritative source of runtime changes — at startup `ListBackends`/`ListPolicies` apply each row (Upsert semantics), and same-name entries override YAML; DB rows referencing routes that no longer exist are skipped with a warning, without blocking startup.

**Cluster coordination**: persistence of a change is performed exactly once by the initiating instance; the other instances only update in-memory state via the Redis broadcast, avoiding duplicate writes.

## Verification

- `store_test.go`: sqlmock verifies SQL branches and parameter binding for both dialects (runs with `make test`);
- `store_live_test.go`: full real-database round trip (create tables / upsert / load / delete); enabled by setting `GATEWAY_TEST_PG_DSN` / `GATEWAY_TEST_MYSQL_DSN`, skipped by default.
