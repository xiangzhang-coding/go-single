---
sidebar_position: 7
---

# payment — 模拟支付

**定位**：不接真实渠道，由内部接口模拟支付成功/失败回调驱动订单状态流转；owner 校验、金额与支付期限核对、payment_id 唯一约束防重复回调。

实现：`internal/payment/`。

## 数据模型

### payments

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 流水 ID |
| payment_id | VARCHAR(64) **UNIQUE** | 支付流水号（**客户端生成**，每次尝试重新生成；唯一约束挡重复回调） |
| order_no | VARCHAR(20) FK | 订单号（RESTRICT） |
| user_id | BIGINT UNSIGNED FK | 支付用户 |
| amount | BIGINT UNSIGNED | 回调申报金额（分），成功回调与订单 `pay_amount` 核对 |
| result | VARCHAR(16) | `success`（驱动订单 待支付→已支付）/ `fail`（订单状态不变，可重试） |

失败流水留档审计；订单停留待支付，客户端以**新 payment_id** 重试。

## 接口

### HTTP（handler/payment_handler.go，Bearer）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | /api/payments/mock | 模拟支付回调 `{order_id, payment_id, amount, result: success\|fail}` |

## 关键流程

### 回调处理

```text
POST /api/payments/mock
  [1] 参数校验（order_id / payment_id / amount / result）
  [2] order.GetDetail（owner 校验——他人订单 403，先于流水检查，防 IDOR 泄露）
  [3] 幂等检查：payment_id 已存在 → 409 重复回调（DB 唯一约束 + 落库 1062 兜底）
  [4] 状态机校验：仅待支付订单可发起支付（成功/失败一致——失败回调不得污染已流转订单）
  [5] 成功回调：金额核对（回调金额 = 订单 pay_amount，不符 409）
  [6] 单事务：创建支付流水 → 成功则 order.MarkPaid 条件更新 待支付→已支付
      （WHERE 同时校验 status、pay_amount 与 expire_at；false = 状态已变、金额不符或已过期 → 回滚整体拒绝）
```

幂等（payment_id 唯一约束）+ 事务原子 ⇒ 基础设施瞬时故障可有限重试 + 退避；业务拒绝（重复/金额不符/非法跃迁）不重试。

跨模块端口：依赖 `order.GetDetail`（owner 校验读取）与 `order.MarkPaid`（事务内条件更新）；进程内调用，见 [order](./order) 模块。

权威源：[docs/DESIGN.md 模拟支付](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000011_payments`。
