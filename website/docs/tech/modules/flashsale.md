---
sidebar_position: 6
---

# flashsale — 秒杀

**定位**：活动管理（时间窗口 / 独立库存 / 秒杀价 / 限购）、上架预热 Redis（SETNX）、限流（全局令牌桶 + 按用户计数）、幂等键 + 类型化缓存原子预扣、发 MQ 异步落单、超时回补与库存对账（进行中只告警、收尾以 MySQL 对齐）。

实现：`internal/flashsale/`。

## 数据模型

### flashsale_activities

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 活动 ID |
| sku_id | BIGINT UNSIGNED FK | 绑定 SKU（RESTRICT） |
| title | VARCHAR(128) | 活动标题 |
| price | BIGINT UNSIGNED | 秒杀价（分） |
| stock | INT UNSIGNED | **活动独立库存**（与 `skus.stock` 互不干扰；落单扣活动库存） |
| per_user_limit | INT UNSIGNED | 每人限购（默认 1） |
| status | VARCHAR(16) | `off_sale`（下架）/ `on_sale`（上架）；**进行中由时间窗口动态判定**（`status=on_sale && start_at <= now <= end_at`），status 仅用于手动下架/紧急停止 |
| start_at / end_at | DATETIME(3) | 时间窗口 |

派生状态（读取时计算，不落库）：用户视角 `not_started` / `in_progress`；admin 视角另含 `off_sale`（手动下架）/ `ended`（窗口已结束）。

## Redis 约定与原子能力

key 约定（活动进行中均为 Redis 预扣事实源）：

| key | 说明 |
| --- | --- |
| `flashsale:stock:{id}` | 活动库存（上架预热；TTL = 结束时间 + 1h 自清理） |
| `flashsale:count:{id}:{user}` | 用户抢购计数（原子预扣 INCR） |
| `flashsale:idem:{id}:{user}` | 幂等键（挡预扣请求重复提交，TTL **30min**） |
| `flashsale:rl:{user}` | 按用户限流计数（固定窗口 INCR+TTL） |

业务 service 只调用类型化缓存能力；Lua 文本与整数返回码协议封装在 `internal/platform/cache` Redis 适配器内：

| 能力 | 语义 |
| --- | --- |
| `WarmFlashSaleStock` | 预热：key 不存在写入；已存在仅当配置库存更低才覆盖（**进行中只减不增**） |
| `PreDeductFlashSale` | 原子预扣：校验 on_sale → 时间窗口 → 库存 → 每人限购 → `DECR 库存 + INCR 用户计数`；返回命名结果而非整数码 |
| `AcquireIdempotency` | 幂等键抢占（内部 SETNX + EX） |
| `RestoreFlashSale` | 回补：库存 key 存在才 INCR、计数 key 存在才 DECR、DEL 幂等键（允许再次抢购） |

## 接口

### HTTP（handler/flashsale_handler.go）

admin（Bearer + admin）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST / PUT | /api/admin/flashsales(/:id) | 创建 / 编辑活动（**进行中编辑库存只减不增**，调高 409） |
| GET | /api/admin/flashsales | 活动列表（全状态 + 派生状态 + SKU/商品摘要） |
| POST | /api/admin/flashsales/:id/publish | 上架（**先预热库存、后写状态**——预热失败保持下架） |
| POST | /api/admin/flashsales/:id/unpublish | 下架（写状态后清除预热库存） |

用户（Bearer）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/flashsales | 秒杀页列表（仅进行中/即将开始；携带 `server_time` 供前端对齐倒计时；剩余库存读 Redis 预扣余量，缺失降级配置库存） |
| POST | /api/flashsales/:id/purchase | 抢购（**全局令牌桶中间件**限流 + 服务内按用户限流；成功返回 **202 排队中 + order_no** 供轮询） |

### 跨模块端口

**对外（实现 order 侧端口，flashsale → order 单向依赖）**：`DeductStock`（ActivityStock：订单事务内条件扣减活动库存）、`RestoreStock` / `RestoreRedis`（SeckillRestore：取消回补）。**依赖**：`product.GetSKU` / `GetProduct`（活动校验与秒杀页摘要）、`order.CreateSeckill` / `order.CountValidSeckill`（消费者与对账）。

## 关键流程

### 抢购全流程（DESIGN.md 秒杀时序）

```text
POST /api/flashsales/:id/purchase
  [1] 全局令牌桶限流（中间件，429）→ 按用户 Redis 固定窗口计数（flashsale:rl:{user}）
  [2] 幂等键抢占（flashsale:idem:{id}:{user}，SETNX + TTL 30min）——先于预扣抢占，
      挡得住重复提交；已存在 → 409 重复请求
  [3] PreDeductFlashSale 类型化原子预扣：
        · 成功 → 保留幂等键
        · 业务拒绝（抢光/限购/窗口外/下架）→ 释放幂等键（允许窗口内重试）
        · 基础设施失败 → 保留幂等键（防瞬时故障下重复预扣）
  [4] 预扣成功 → 生成雪花订单号 → 发布 MQ"抢购成功"消息
      （flashsale.order.create 队列，发布确认 + 有限重试；发布失败保留幂等键，对账兜底）
  [5] 返回 202 {"status":"queued","order_no":...}，前端轮询 GET /api/orders/{order_no}
```

### 异步落单消费者（flashsale.order.create）

```text
消息 {order_no, user_id, activity_id}
  → 查活动（sku_id/秒杀价为订单快照来源；不存在 → 永久失败进死信）
  → 查默认地址（user.GetDefaultAddress，固化为地址快照；无地址 → 永久失败进死信）
  → order.CreateSeckill：单事务建秒杀订单（10min 超时）+ 订单项 + 条件扣活动库存
      · 重复键（order_no 主键 / user_activity_key 唯一约束）→ 幂等成功（不重复扣减库存）
      · 活动库存不足 → 永久失败（死信）
      · DB 瞬时故障 → 重投（Nack requeue，at-least-once）
```

### 对账（cron）

| 任务 | 频率 | 语义 |
| --- | --- | --- |
| flashsale-reconcile-active | 每小时 | 进行中活动：比对 Redis 库存 vs MySQL 库存 vs 秒杀有效订单数，**只告警不写回**；`redis < mysql` 识别为"有预扣无订单"补单/回补信号 |
| flashsale-reconcile-ended | 每分钟 | 刚过 end_at 的上架活动：**以 MySQL 为准 SET 对齐 Redis**（key 缺失仅结束 30min 窗口内回建；下架活动不回建） |

权威源：[docs/DESIGN.md 秒杀时序 / 秒杀活动 / 定时任务](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000008_flashsale_activities`。对账细节亦见[运营域](../domains/operations)。
