---
sidebar_position: 6
---

# 运营域

**术语**：对账（定义见 [CONTEXT.md](https://github.com/xiangzhang-coding/go-single/blob/main/CONTEXT.md)）。

**模块映射**：

| 模块 | 承担 |
| --- | --- |
| [flashsale](../modules/flashsale) | 定时比对 Redis 库存 / MySQL 库存 / 秒杀有效订单数；进行中只告警，收尾以 MySQL 对齐 Redis |

**说明**：薄视图页。对账细节见 flashsale 模块页与 `internal/flashsale/`（reconciliation 与收尾对账 cron）。
