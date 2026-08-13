---
sidebar_position: 8
---

# coupon — 优惠券

**定位**：券模板（直减/满减，admin 发布）、领券（Lua 原子脚本防超发）、我的券、下单核销与取消回退；与秒杀互斥。

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

## Redis 约定与 Lua 脚本

| key | 说明 |
| --- | --- |
| `coupon:claimed:{template_id}` | 总量计数 |
| `coupon:peruser:{template_id}:{user_id}` | 每人限领计数 |

`claimScript`（复用秒杀 Lua 模式防超发）：校验有效期窗口 → 检查总量 → 检查每人限领 → 双计数 INCR。返回 1 成功 / 0 已抢光 / -1 不在有效期 / -2 超过每人限领。

## 接口

### HTTP（handler/coupon_handler.go）

用户（Bearer）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/coupons | 可领券列表（含当前用户视角状态：`claimable` / `not_started` / `ended` / `sold_out` / `limit_reached`；计数取 DB 仅作展示，防超发强制在 Lua） |
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
  → DB 读模板（含有效期快照；不存在 → 404）
  → Lua 原子计数（claimScript）——模板状态/总量/限领的并发条件全部 Redis 内强制
  → DB 落库 user_coupons（unused）为最终态
```

### 下单核销与回退（order 事务内）

```text
下单：GetUsable 校验 → 事务内 UseCoupon（条件更新，并发仅一次成功）
     → 订单记 discount_amount，应付 = 总额 − 券额（满减门槛在结算时校验，门槛不足 409）
取消：事务内 RollbackCoupon（used→unused 回退）——与取消状态迁移同事务
```

互斥：秒杀订单不使用优惠券（order_type=seckill 建单不带 coupon_id）。

权威源：[docs/DESIGN.md 优惠券](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000004_coupons`。
