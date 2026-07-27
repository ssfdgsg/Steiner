# scripts/ — 本地验证脚本

## 职责
承载「本地可重复验证」要求的脚本，全部可离线运行（不依赖 CI）。

## 文件规划
| 文件 | 说明 |
|---|---|
| `smoke.sh` | 冒烟：编译 → 后台拉起 2 个 mock 后端（vllm/sglang 类型）→ 拉起网关（minimal 配置）→ 依次验证：非流式补全、流式补全、`/admin/backends` 健康视图、`PUT /admin/policy` 热更生效、429 限流 → 全部断言通过输出 PASS，任一失败非零退出并打印上下文 |
| `bench_policy.sh`（计划） | 表达式求值与调度热路径基准，输出与目标值（单候选 < 5µs）的对比 |
| `loadtest.sh`（计划） | 基于 vegeta/hey 的混合流量压测（长短 prompt、流式比例、会话复用率可调） |

## 约定
- 脚本统一 `set -euo pipefail`；
- 端口从 18080 起用高位段，避免与常用服务冲突；退出 trap 清理全部子进程；
- 冒烟结果追加记录到 `.claude/verification-report.md`（时间戳 + 通过项清单）。
