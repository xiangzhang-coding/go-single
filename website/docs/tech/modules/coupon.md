---
sidebar_position: 8
---

# coupon — 优惠券

**定位**：券模板（直减/满减，admin 发布）、领券（可重建 Redis 计数 + MySQL 事务硬约束）、我的券、下单核销与取消回退；与秒杀互斥。

实现：`internal/coupon/`。

## 数据模型

### coupon_templates（券模板，admin 发布）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 模板 ID |
| name | VARCHAR(64) | 模板名 |
| type | VARCHAR(16) | `direct`（直减：满 0 减面额）/ `threshold`（满减：满 min_amount 减面额） |
| value | BIGINT UNSIGNED | 面额（分） |
| min_amount | BIGINT UNSIGNED | 满减门槛（分；直减为 0，校验门槛 ≥ 面额） |
| total | INT UNSIGNED | 发放总量 |
| per_user_limit | INT UNSIGNED | 每人限领（默认 1） |
| valid_from / valid_until | DATETIME(3) | 有效期 |

### user_coupons（用户持有的券）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 领取记录 ID |
| user_id / template_id | BIGINT UNSIGNED FK | 领取人与模板（CASCADE） |
| status | VARCHAR(16) | `unused` / `used`；**`expired` 由读取时按有效期派生**（未用且已过 valid_until），不落库 |

## Redis 约定与原子能力

| key | 说明 |
| --- | --- |
| `coupon:claimed:{template_id}` | 总量计数 |
| `coupon:peruser:{template_id}:{user_id}` | 每人限领计数 |
| `coupon:version:{template_id}` | 总计数已同步的 MySQL 版本 |
| `coupon:peruser-version:{template_id}:{user_id}` | 当前用户计数已同步的 MySQL 版本 |

业务 service 调用 `ClaimCoupon` 触发类型化计数；缓存适配器内部以 Lua 先把缺失或落后的计数抬升到 MySQL 已领数，再校验有效期窗口 → 检查总量 → 检查每人限领 → 双计数 INCR，并把原始返回码封装为 `CouponClaimed` / `CouponSoldOut` / `CouponNotInWindow` / `CouponLimitReached`。最终领取结果由 MySQL 事务裁决，且有效期时间在获取模板行锁后采样。每次数据库裁决后调用 `SyncCouponCounts`，总计数与当前用户计数分别以自身的 MySQL 计数为单调版本按数据库事实覆盖，延迟到达的旧快照不会阻塞另一用户的计数修复。

## 接口

### HTTP（handler/coupon_handler.go）

用户（Bearer）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/coupons | 可领券列表（含当前用户视角状态：`claimable` / `not_started` / `ended` / `sold_out` / `limit_reached`；计数取 DB 事实） |
| POST | /api/coupons/:id/claim | 领取（成功 201 返回用户券） |
| GET | /api/coupons/mine | 我的券（status 筛选：unused/used/expired + 分页） |

admin（Bearer + admin）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST / PUT | /api/admin/coupons(/:id) | 发布 / 编辑券模板 |
| GET | /api/admin/coupons | 模板列表（含已领数 `claimed_count`，供后台展示 已领/总量） |

### 跨模块端口（供 order 结算事务）

| 端口 | 说明 |
| --- | --- |
| `GetUsable` | 结算前校验：归属当前用户 / 未用 / 在有效期（门槛校验由 order 按订单总额完成——全场券，无商品维度限制） |
| `UseCoupon` | 事务内条件核销（unused→used + 有效期窗口原子校验，并发仅一次成功；失败重查区分已用/过期/不存在） |
| `RollbackCoupon` | 事务内条件回退（used→unused），取消订单时调用 |

## 关键流程

### 领券

```text
POST /api/coupons/:id/claim
  → DB 读模板与已领数（不存在 → 404），作为 Redis 重建基线
  → ClaimCoupon 类型化原子计数；缓存丢失时先从 DB 事实重建
  → MySQL 锁定模板行，在同一事务内重查有效期/总量/限领并落库 user_coupons（unused）
  → Redis 拒绝/故障不单独决定领取结果；与事务结果不一致时按 DB 事实修复双计数
```

### 下单核销与回退（order 事务内）

```text
下单：GetUsable 校验 → 事务内 UseCoupon（条件更新，并发仅一次成功）
     → 订单记 discount_amount，应付 = 总额 − 券额（满减门槛在结算时校验，门槛不足 409）
取消：事务内 RollbackCoupon（used→unused 回退）——与取消状态迁移同事务
```

互斥：秒杀订单不使用优惠券（order_type=seckill 建单不带 coupon_id）。

权威源：[docs/DESIGN.md 优惠券](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000004_coupons`。
