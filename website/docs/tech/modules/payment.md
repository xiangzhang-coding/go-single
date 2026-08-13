---
sidebar_position: 7
---

# payment — 模拟支付

**定位**：不接真实渠道，由内部接口模拟支付成功/失败回调驱动订单状态流转；owner 校验、金额核对、payment_id 唯一约束防重复回调。

**状态**：占位。数据模型 / 接口 / 时序待后续填充（实现：`internal/payment/`）。

领域：见[交易域](../domains/trade)。
