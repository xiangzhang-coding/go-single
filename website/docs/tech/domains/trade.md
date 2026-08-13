---
sidebar_position: 3
---

# 交易域

**术语**：订单 / 地址簿 / 地址快照 / 秒杀活动 / 预扣 / 模拟支付 / 优惠券（定义见 [CONTEXT.md](https://github.com/xiangzhang-coding/go-single/blob/main/CONTEXT.md)）。

**模块映射**：

| 模块 | 承担 |
| --- | --- |
| [order](../modules/order) | 订单生命周期（待支付→已支付→已发货→已完成，含取消/超时取消）、地址快照、幂等 |
| [flashsale](../modules/flashsale) | 秒杀活动、预扣、异步落单（对账见[运营域](./operations)） |
| [payment](../modules/payment) | 模拟支付回调驱动状态流转 |
| [coupon](../modules/coupon) | 券模板 / 领券 / 核销与回退（与秒杀互斥） |

**说明**：薄视图页。细节见各模块页与 `internal/order/`、`internal/flashsale/`、`internal/payment/`、`internal/coupon/`。
