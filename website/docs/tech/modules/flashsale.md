---
sidebar_position: 6
---

# flashsale — 秒杀

**定位**：活动管理（时间窗口 / 独立库存 / 秒杀价 / 限购）、上架预热 Redis、限流、类型化缓存原子预扣、持久预扣生命周期、MQ 异步落单、可重试回补与逐预扣事实对账。

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

### flashsale_pre_deductions

每次抢购按 `(user_id, activity_id, client_request_id)` 幂等创建稳定 `id`，该 ID 同时是购买槽位。表内固化 SKU、成交价、数量、槽位和订单号；状态流为 `preparing → pending_publish → pending_order → ordered → pending_rollback → rolled_back`。该表同时承担本地 saga 与 MQ outbox 事实源。

## Redis 约定与原子能力

key 约定（活动进行中均为 Redis 预扣事实源）：

| key | 说明 |
| --- | --- |
| `flashsale:stock:{id}` | 活动库存（上架预热；TTL = 结束时间 + 1h 自清理） |
| `flashsale:count:{id}:{user}` | 用户抢购计数（原子预扣 INCR） |
| `flashsale:idem:{id}:{user}:{purchase_slot}` | 槽位所有权键（TTL **30min**；不同合法购买互不覆盖） |
| `flashsale:reservation:{pre_deduction_id}` | 与库存/计数同一 Lua 写入的持久预扣标记；回退先写 `rolled_back` tombstone，MySQL 终态提交后再清理，重试可区分“已回补”与“标记异常缺失” |
| `flashsale:pause:{id}` | 进行中库存编辑的短暂预扣栅栏；先封口、再锁活动行重算差额，避免编辑与消费者/新预扣竞态 |
| `flashsale:rl:{user}` | 按用户限流计数（固定窗口 INCR+TTL） |

业务 service 只调用类型化缓存能力；Lua 文本与整数返回码协议封装在 `internal/platform/cache` Redis 适配器内：

| 能力 | 语义 |
| --- | --- |
| `WarmFlashSaleStock` | 预热：key 不存在写入；已存在仅当配置库存更低才覆盖（**进行中只减不增**） |
| `PauseFlashSaleStockDurably` / `DecreaseFlashSaleStockDurably` | 进行中库存编辑先暂停新预扣并读取可售量，锁行校验后事务提交新库存并临时下架，再按锁内差额扣 Redis；不得低于已接受预扣，AOF 未确认则保持下架 |
| `PreDeductFlashSaleDurably` | 原子预扣后在同一专用连接执行 `WAITAOF`；同一 marker 重放会重写当前状态快照后再次确认 AOF |
| `EnsureFlashSaleReservationDurably` | 发布/重投前验证 marker；若整次 Lua 结果丢失，则按 MySQL 持久事实原子重建库存、用户计数、幂等键与 marker，并等待 AOF fsync |
| `EnsureOrderedFlashSaleReservationDurably` | ordered 事实按 MySQL 活动库存校准 Redis，并重建用户计数/幂等键/marker；AOF 确认后才允许清理 marker |
| `AcquireIdempotency` | 幂等键抢占（内部 SETNX + EX） |
| `RestoreFlashSale` | 仅 reservation token 匹配时回补并写 tombstone；重复执行不多回补，标记缺失不假报成功，且 compare-delete 不会删除新请求幂等键 |

## 接口

### HTTP（handler/flashsale_handler.go）

admin（Bearer + admin）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST / PUT | /api/admin/flashsales(/:id) | 创建 / 编辑活动；进行中仅标题和库存减少可改，SKU/价格/时间/限购锁定；Redis 同步失败自动下架 |
| GET | /api/admin/flashsales | 活动列表（全状态 + 派生状态 + SKU/商品摘要） |
| POST | /api/admin/flashsales/:id/publish | 上架（**先预热库存、后写状态**——预热失败保持下架） |
| POST | /api/admin/flashsales/:id/unpublish | 下架（写状态后清除预热库存） |

用户（Bearer）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/flashsales | 秒杀页列表（仅进行中/即将开始；携带 `server_time` 供前端对齐倒计时；剩余库存读 Redis 预扣余量，缺失降级配置库存） |
| POST | /api/flashsales/:id/purchase | 请求体 `{client_request_id}`；成功返回 **202 排队中 + pre_deduction_id + 可选 order_no**；同请求 ID 重试返回原生命周期，新请求 ID 在限购内分配新槽位 |
| GET | /api/flashsales/purchases/:id | 查询本人预扣生命周期（owner 校验；他人记录返回 404） |

### 跨模块端口

**应用编排（flashsale → order 单向依赖）**：消费者通过 flashsale `TxRunner` 开启事务，调用 `order.CreateSeckillInTx` 后由活动仓储条件扣减库存；超时取消通过 `order.ListExpiredSeckill` / `CancelSeckill` 与活动仓储完成事务内取消和 MySQL 回补，提交后 `RestoreRedis`；对账调用 `order.CountValidSeckill`。**其他依赖**：`product.GetSKU` / `GetProduct`（活动校验与秒杀页摘要）。order 不反向持有 flashsale 实例。

## 关键流程

### 抢购全流程（DESIGN.md 秒杀时序）

```text
POST /api/flashsales/:id/purchase
  [1] 全局令牌桶限流（中间件，429）→ 按用户 Redis 固定窗口计数（flashsale:rl:{user}）
  [2] MySQL 按 client_request_id 创建 preparing 事实，固化 SKU/价格/数量，pre_deduction_id 作为购买槽位
  [3] PreDeductFlashSale 类型化原子预扣：
        · 成功 → 库存/计数/reservation marker 同时提交，状态转 pending_publish
        · 业务拒绝（抢光/限购/窗口外/下架）→ 释放幂等键（允许窗口内重试）
        · 基础设施失败 → 保留幂等键（防瞬时故障下重复预扣）
  [4] 持久化雪花订单号 → 发布 MQ；确认成功转 pending_order
      订单号生成/发布失败由启动恢复与每分钟任务继续；重投前验证或重建 Redis reservation；仅 pending_publish 10 次仍失败转回退，pending_order 不因补发失败误回退
  [5] 返回 202，前端按 pre_deduction_id 轮询 ordered / rolled_back
```

### 异步落单消费者（flashsale.order.create）

```text
消息 {pre_deduction_id, order_no, user_id, activity_id, sku_id, price, quantity, purchase_slot}
  → 与持久预扣快照逐字段校验（活动编辑不能改写已接受成交语义）
  → 查默认地址（user.GetDefaultAddress，固化为地址快照；无地址 → 永久失败进死信）
  → flashsale 编排开启事务：order.CreateSeckillInTx 建秒杀订单（10min 超时）+ 订单项
      → 活动仓储条件扣活动库存
      · 重复键（order_no 主键 / user_activity_key=user:activity:slot）→ 同槽幂等成功（不重复扣减库存）
      · 活动库存不足 → 永久失败（死信）
      · DB 瞬时故障 → 重投（Nack requeue，at-least-once）
      · 永久失败 → 持久化 pending_rollback 后进 DLQ；死信消费者确认回退意图
```

### 对账（cron）

| 任务 | 频率 | 语义 |
| --- | --- | --- |
| flashsale-pre-deduction-recovery | 启动时 + 每分钟 | 扫描 preparing/pending_publish/pending_order/pending_rollback，继续发布、重投或幂等回退 |
| flashsale-reservation-cleanup | 每分钟 | 查询 ordered 对应订单状态；离开待支付态后清理无需再补偿的持久 marker，并记录清理时间防重复扫描 |

pre-R04 durable 主队列或 DLQ 消息没有 `pre_deduction_id` 时，会先按 `order_no` 创建/复用 legacy 生命周期，再进入同一落单和回退状态机。
| flashsale-reconcile-active | 每小时 | 输出具体 pre_deduction_id/user_id/order_no/status，并保留聚合库存差异作为最终不变量告警 |
| flashsale-reconcile-ended | 每分钟 | 无未决预扣事实时才以 MySQL 对齐 Redis，避免随后逐笔回退造成多回补 |

权威源：[docs/DESIGN.md 秒杀时序 / 秒杀活动 / 定时任务](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000008_flashsale_activities` / `000018_flashsale_purchase_slots`。对账细节亦见[运营域](../domains/operations)。
