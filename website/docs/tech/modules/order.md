---
sidebar_position: 5
---

# order — 订单

**定位**：购物车结算 / 直购下单（单事务：订单 + 订单项 + 库存扣减 + 地址快照 + 券核销 + 清购物车）、`client_request_id` 幂等、雪花订单号、状态机（待支付→已支付→已发货→已完成，含取消与超时取消），并向 flashsale 编排提供事务内秒杀订单能力。

实现：`internal/order/`。

## 数据模型

### orders

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| order_no | VARCHAR(20) PK | **雪花 ID** 十进制字符串（避免 JS 精度丢失，JSON 按字符串序列化） |
| user_id | BIGINT UNSIGNED FK | 下单用户（CASCADE） |
| client_request_id | VARBINARY(64) NULL | 普通订单持久请求身份；与 user_id 联合唯一，秒杀/历史订单为 NULL |
| order_type | VARCHAR(16) | `normal` / `seckill`（秒杀订单不使用优惠券） |
| status | VARCHAR(16) | 状态机（见下） |
| activity_id | BIGINT UNSIGNED NULL | 秒杀活动（秒杀订单专属，FK RESTRICT） |
| purchase_slot | BIGINT UNSIGNED NULL | 秒杀预扣分配的稳定购买槽位 |
| total_amount / discount_amount / pay_amount | BIGINT UNSIGNED | 总额 − 券额 = 应付（分），关系由应用预检与数据库 CHECK 双重保证 |
| coupon_id | BIGINT UNSIGNED NULL | 核销的用户券（FK RESTRICT） |
| receiver / phone / province / city / district / detail | VARCHAR | **地址快照**（下单时从地址簿固化，后续改地址不影响历史订单） |
| paid_at / shipped_at / completed_at / cancelled_at | DATETIME(3) NULL | 各状态时间戳 |
| expire_at | DATETIME(3) | 超时取消时间（普通 **15min**；秒杀 **10min**） |
| user_activity_key | VARCHAR(64) NULL UNIQUE | **秒杀槽位去重键**：落单写 `user_id:activity_id:purchase_slot`，取消同事务置 NULL；不同槽位可并存，同槽消息重投只命中一单 |

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
| price / quantity / subtotal | — | 成交单价快照 / 数量 / 小计；价格上限 100,000,000 分，`subtotal = price × quantity` 由 CHECK 保证 |

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

**依赖（order 侧声明最小接口）**：`ProductService`（SKU 读取/条件扣减/回补）、`CouponService`（GetUsable/UseCoupon/RollbackCoupon）、`CartService`（LockItems/DeletePurchased）、`UserService`（GetAddress 快照）。order 不持有 flashsale service 或活动仓储。

**对外端口（实现方为 order，被其他模块消费）**：`CreateSeckillInTx`（flashsale 消费者事务内建订单与订单项）、`ListExpiredSeckill` / `CancelSeckill`（flashsale 超时取消编排）、`CountValidSeckill`（flashsale 对账）、`MarkPaid`（payment 事务内条件更新）、`GetDetail`（payment owner 校验）、`HasPurchasedSKU`（social 动态分享校验）。

## 关键流程

### 普通下单（单事务 + 幂等）

```text
POST /api/orders {client_request_id, address_id, ...}
  [1] 参数校验（from_cart 与 items 互斥；直购同 SKU 多行合并，数量 1–99）
  [2] 按 (user_id, client_request_id) 查询 MySQL 持久事实；命中直接返回原订单
  [3] 未命中时生成雪花订单号 + 类型化缓存能力 AcquireIdempotency 原子抢占
      order:idem:{uid}:{crid}（内部 SETNX + TTL 15min；已存在 → 返回既有订单号，
      未落库则 202 轮询）
  [4] 读地址（固化为快照）→ 组装订单项（购物车 LockItems / 直购）→ 校验券可用
  [5] 单事务：
        · 按 商品→SKU 固定顺序锁定并读取（GetSKUForUpdate），检查乘法与累加
        · 在任何写入前验证总额、优惠额、应付额、逐项小计与订单项合计
        · 条件扣减库存（stock >= N）
        · 核销券（UseCoupon 条件更新 unused→used）
        · 建订单 + 订单项（含地址快照；唯一键处理并发请求身份）
        · 删除已结算购物车条目（DeletePurchased）
  [6] 校验类失败 → 删除幂等键（允许修正后重试）；基础设施失败先查订单，
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
flashsale.SeckillTimeout（cron 每分钟 seckill-timeout-cancel）
  → order.ListExpiredSeckill 扫描待支付秒杀订单
  → 事务：order.CancelSeckill 条件更新取消 + flashsale 活动仓储回补 MySQL 库存
  → 事务提交后按订单槽位回补 Redis（库存按数量 INCR + 用户槽位计数 DECR 1 + 释放对应槽位键；
    best-effort，失败由对账 cron 兜底）
```

### 支付状态迁移（payment 模块事务内调用）

```text
MarkPaid(tx, orderNo, payAmount)
  → UPDATE orders SET status='paid' WHERE order_no=? AND status='pending_payment'
    AND pay_amount=? AND expire_at>?   ← 状态、金额与期限由同一 WHERE 原子兜底
  false = 状态已变、金额不符或订单过期（支付流水整体回滚）
```

权威源：[docs/DESIGN.md 订单状态机 / 普通订单流程](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000009_orders` / `000010_order_foreign_keys` / `000014_seckill_repurchase` / `000019_order_payment_invariants`。
