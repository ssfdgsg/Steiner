# docs/ — 架构决策记录（ADR）

## 职责
存放**决策及其理由**（为什么这样做），与各目录 README（是什么、怎么用）分层：模块细节永远以模块 README 为唯一来源，本目录只记录跨模块的取舍过程，避免内容重复。

## ADR 索引
| 编号 | 标题 | 状态 |
|---|---|---|
| `adr-0001-tech-stack.md` | 技术选型：expr 表达式引擎、官方 Prometheus 库、标准库 HTTP、自研 radix tree 特例 | 已接受 |
| `adr-0002-dual-metrics-channel.md`（计划） | 为什么直采与 PromQL 双通道并存而非二选一 | 草案 |
| `adr-0003-distributed-state.md`（计划） | 多副本网关的会话粘性与限流配额共享（Redis vs LB 一致性哈希） | 待决 |

## ADR 模板
```markdown
# ADR-NNNN：标题
- 状态：草案 | 已接受 | 已废弃（被 ADR-XXXX 取代）
- 日期：YYYY-MM-DD
## 背景（面临什么问题）
## 决策（选了什么）
## 备选与否决理由
## 后果（正负影响、迁移/回滚方式）
```
