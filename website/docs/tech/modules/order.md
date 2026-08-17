---
sidebar_position: 5
---

# order — 订单

**定位**：购物车结算 / 直购下单（单事务：订单 + 订单项 + 库存扣减 + 地址快照 + 券核销 + 清购物车）、`client_request_id` 幂等、雪花订单号、状态机（待支付→已支付→已发货→已完成，含取消与超时取消）、秒杀异步落单与超时回补。

实现：`internal/order/`。

## 数据模型

### orders

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| order_no | VARCHAR(20) PK | **雪花 ID** 十进制字符串（避免 JS 精度丢失，JSON 按字符串序列化） |
| user_id | BIGINT UNSIGNED FK | 下单用户（CASCADE） |
| order_type | VARCHAR(16) | `normal` / `seckill`（秒杀订单不使用优惠券） |
| status | VARCHAR(16) | 状态机（见下） |
| activity_id | BIGINT UNSIGNED NULL | 秒杀活动（秒杀订单专属，FK RESTRICT） |
| total_amount / discount_amount / pay_amount | BIGINT UNSIGNED | 总额 − 券额 = 应付（分） |
| coupon_id | BIGINT UNSIGNED NULL | 核销的用户券（FK RESTRICT） |
| receiver / phone / province / city / district / detail | VARCHAR | **地址快照**（下单时从地址簿固化，后续改地址不影响历史订单） |
| paid_at / shipped_at / completed_at / cancelled_at | DATETIME(3) NULL | 各状态时间戳 |
| expire_at | DATETIME(3) | 超时取消时间（普通 **15min**；秒杀 **10min**） |
| user_activity_key | VARCHAR(64) NULL UNIQUE | **秒杀去重键**（T13）：落单写 `user_id:activity_id`，取消同事务置 NULL——MySQL 唯一索引允许多个 NULL，取消后不占位、允许再次抢购；非取消订单仍唯一挡重复落单 |

状态机（`model.CanTransition`，仅合法跃迁，非法一律拒绝）：

```text
pending_payment(待支付) ──支付回调──▶ paid(已支付) ──后台发货──▶ shipped(已发货) ──确认收货──▶ completed(已完成)
        │
        └──用户取消 / 超时取消──▶ cancelled(已取消)
```

### order_items

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 订单项 ID |
| order_no | VARCHAR(20) FK | 所属订单（CASCADE） |
| sku_id | BIGINT UNSIGNED FK | SKU（**RESTRICT**：有订单历史不可删） |
| product_id / title / specs | — | 商品快照（标题/规格） |
| price / quantity / subtotal | — | 成交单价快照 / 数量 / 小计（**价格不受后续改价影响**） |

## 接口

### HTTP（handler/order_handler.go）

用户（Bearer）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | /api/orders | 下单 `{client_request_id, address_id, coupon_id?, from_cart 或 items[]}`；创建 201 / 幂等命中 200 / 幂等键占用未落库 **202**（客户端轮询详情） |
| GET | /api/orders | 我的订单（status 筛选 + 分页） |
| GET | /api/orders/:order_no | 订单详情（owner 校验） |
| POST | /api/orders/:order_no/cancel | 取消待支付订单（**秒杀订单拒绝**——走超时取消路径） |
| POST | /api/orders/:order_no/confirm | 确认收货（已发货 → 已完成，owner 校验） |

admin（Bearer + admin）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/admin/orders | 后台订单列表（**全量跨用户**，status 筛选 + 分页） |
| POST | /api/admin/orders/:order_no/ship | 后台发货（已支付 → 已发货） |

### 跨模块端口（进程内调用）

**依赖（order 侧声明最小接口）**：`ProductService`（SKU 读取/条件扣减/回补）、`CouponService`（GetUsable/UseCoupon/RollbackCoupon）、`CartService`（LockItems/DeletePurchased）、`UserService`（GetAddress 快照）、`ActivityStock` 与 `SeckillRestore`（**由 flashsale 实现**——秒杀落单扣活动库存 / 取消回补）。

**对外端口（实现方为 order，被其他模块消费）**：`CreateSeckill`（flashsale 消费者）、`MarkPaid`（payment 事务内条件更新）、`GetDetail`（payment owner 校验）、`CountValidSeckill`（flashsale 对账）、`HasPurchasedSKU`（social 动态分享校验）。

## 关键流程

### 普通下单（单事务 + 幂等）

```text
POST /api/orders {client_request_id, address_id, ...}
  [1] 参数校验（from_cart 与 items 互斥；直购同 SKU 多行合并，数量 1–99）
  [2] 生成雪花订单号 + 类型化缓存能力 AcquireIdempotency 原子抢占
      order:idem:{uid}:{crid}（内部 SETNX + TTL 15min；已存在 → 返回既有订单号，
      未落库则 202 轮询）
  [3] 读地址（固化为快照）→ 组装订单项（购物车 LockItems / 直购）→ 校验券可用
  [4] 单事务：
        · 按 商品→SKU 固定顺序锁定并读取（GetSKUForUpdate），累计总额
        · 条件扣减库存（stock >= N）
        · 核销券（UseCoupon 条件更新 unused→used）
        · 建订单 + 订单项（含地址快照）
        · 删除已结算购物车条目（DeletePurchased）
  [5] 校验类失败 → 删除幂等键（允许修正后重试）；基础设施失败先查订单，
      未提交才释放（防瞬时故障下重试生成第二单）
```

### 取消与超时取消

```text
Cancel（用户取消，仅普通订单）
  → 事务：条件更新 待支付→已取消（RowsAffected=0 即状态已变，不重复回补）
        + RestoreStock 回补库存 + RollbackCoupon 回退券
CancelExpired（cron 每分钟 order-timeout-cancel，批量上限 500）
  → 扫描待支付且已过 expire_at 的普通订单 → 逐个同事务取消（同上）
  → 单订单失败跳过计失败数，下个 tick 重试（不阻断整轮）
CancelExpiredSeckill（cron 每分钟 seckill-timeout-cancel）
  → 扫描待支付秒杀订单 → 事务：条件更新取消 + 回补活动 MySQL 库存
  → 事务提交后回补 Redis（库存 INCR + 用户计数 DECR + 释放幂等键，允许再次抢购；
    best-effort，失败由对账 cron 兜底）
```

### 支付状态迁移（payment 模块事务内调用）

```text
MarkPaid(tx, orderNo, payAmount)
  → UPDATE orders SET status='paid' WHERE order_no=? AND status='pending_payment'
    AND pay_amount=?   ← 状态机与金额核对由条件更新 WHERE 原子兜底
  false = 状态已变或金额不符（整体回滚）
```

权威源：[docs/DESIGN.md 订单状态机 / 普通订单流程](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000009_orders` / `000010_order_foreign_keys` / `000014_seckill_repurchase`。
