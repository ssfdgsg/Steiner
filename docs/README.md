# docs/ — 跨模块设计与架构文档

## 职责
存放跨模块的产品设计基线与**架构决策及其理由**。各目录 README 继续说明模块是什么、怎么用；本目录记录需要多个模块共同遵循的需求、约束与取舍，避免内容重复。

## 产品与设计规范
| 文档 | 状态 | 说明 |
|---|---|---|
| [`frontend-console-redesign-plan.md`](frontend-console-redesign-plan.md) | 设计与实施基线 | 基于七张视觉参考图和当前 Admin API 的管理控制台重设计目标、页面规范与验收标准 |

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
