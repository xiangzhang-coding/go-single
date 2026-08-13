---
sidebar_position: 6
---

# flashsale — 秒杀

**定位**：活动管理（时间窗口 / 独立库存 / 秒杀价 / 限购）、上架预热 Redis（SETNX）、限流（全局令牌桶 + 按用户计数）、幂等键 + Lua 原子预扣、发 MQ 异步落单、超时回补与库存对账（进行中只告警、收尾以 MySQL 对齐）。

**状态**：占位。数据模型 / 接口 / 时序待后续填充（实现：`internal/flashsale/`）。

领域：见[交易域](../domains/trade)与[运营域](../domains/operations)。
