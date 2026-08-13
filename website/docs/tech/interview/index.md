---
sidebar_position: 1
---

# 面试题库 · 技术文档

**12 分章 × 7 题 = 84 题**，覆盖 Go 后端就业面试主流考点；每题包含：

1. **答案要点**——直接可背的得分点；
2. **可运行 Go 代码**——独立程序，`go run ./interview/<章>/<题>` 即可运行（代码在仓库根目录 `interview/`，与文档一一对应，经 `go build ./...` 与抽样运行验证）；
3. **项目位置**——关联本项目真实实现，面试时可指着仓库讲。

| 章节 | 主题 | 代码目录（章，每题一个 `q*` 子目录） |
| --- | --- | --- |
| [01 Go 基础](./ch01-go-basics) | slice/接口/错误/JSON 等语言核心 | `interview/ch01_go_basics/` |
| [02 并发](./ch02-concurrency) | goroutine/channel/锁/原子操作 | `interview/ch02_concurrency/` |
| [03 网络](./ch03-network) | 中间件/JWT/CORS/WS/优雅关闭 | `interview/ch03_network/` |
| [04 MySQL](./ch04-mysql) | 事务/锁/唯一键/连接池 | `interview/ch04_mysql/` |
| [05 Redis](./ch05-redis) | 缓存/Lua/TTL/降级 | `interview/ch05_redis/` |
| [06 MQ](./ch06-mq) | RabbitMQ 确认/重投/死信/熔断 | `interview/ch06_mq/` |
| [07 秒杀架构](./ch07-flashsale) | 限流/预扣/异步落单/对账 | `interview/ch07_flashsale/` |
| [08 认证安全](./ch08-auth-security) | JWT/bcrypt/RBAC/上传安全 | `interview/ch08_auth/` |
| [09 工程实践](./ch09-engineering) | 模块化单体/分层/配置/测试 | `interview/ch09_engineering/` |
| [10 部署运维](./ch10-deploy-ops) | Compose/Nginx/迁移/发布 | `interview/ch10_deploy/` |
| [11 可观测性](./ch11-observability) | 指标/日志聚合/健康检查 | `interview/ch11_observability/` |
| [12 容错与降级](./ch12-resilience) | 超时/退避/熔断/兜底 | `interview/ch12_resilience/` |

## 使用建议

- 先自己答，再看**答案要点**；代码只是演示语义，生产实现以 `internal/` 为准。
- 每个"项目位置"都可 `grep` 定位：`rg -n "函数名" internal/`。
- 章目录下每个 `q*` 子目录是一个独立可运行程序，例如 `go run ./interview/ch01_go_basics/q01_slice_growth`（完整清单见仓库根 `interview/README.md`）。
- 配套业务文档：[技术文档·模块](../modules/)、[领域视图](../domains/)、[用户指南](../../user-guide/feature-guide)。
