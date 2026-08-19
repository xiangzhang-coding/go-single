---
sidebar_position: 3
---

# product — 商品

**定位**：类目 / 商品（SPU）/ SKU（规格、价格、库存）；admin 维护，游客浏览（详情走缓存）。

实现：`internal/product/`。

## 数据模型

### categories

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 类目 ID |
| name | VARCHAR(64) UNIQUE | 类目名 |

### products（SPU）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 商品 ID |
| category_id | BIGINT UNSIGNED FK | 类目（RESTRICT：有商品不可删类目） |
| title | VARCHAR(128) | 标题 |
| description | TEXT | 详情 |
| status | VARCHAR(16) | `off_sale`（下架/草稿，游客不可见）/ `on_sale` |

### skus

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | SKU ID |
| product_id | BIGINT UNSIGNED FK | 所属商品（CASCADE） |
| specs | VARCHAR(255) | 规格组合 JSON（如 `{"color":"红","size":"M"}`） |
| price | BIGINT UNSIGNED | 售价（分），应用与数据库共同限制为 0–100,000,000 分（100 万元） |
| stock | INT UNSIGNED | 普通订单库存 |

约束注意：`skus` 被 `order_items`、`flashsale_activities`、`posts` 以 FK RESTRICT 引用——**有订单/活动/动态历史的 SKU 不可删除**（历史可追溯）。

## 接口

### HTTP（handler/product_handler.go）

游客浏览（无鉴权）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/categories | 类目列表 |
| GET | /api/products | 上架商品列表（`category_id` 筛选 + `page`/`page_size`，默认 20 上限 50） |
| GET | /api/products/:id | 商品详情（**仅上架**，下架/不存在一律 404） |

admin 管理（Bearer + admin 角色）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST / PUT / DELETE | /api/admin/categories(/:id) | 类目增改删（删类目前置检查无商品，否则 409） |
| POST / PUT | /api/admin/products(/:id) | 商品增改（新建默认**下架**） |
| GET | /api/admin/products | 后台商品列表（`status` 筛选，含草稿/下架） |
| POST | /api/admin/products/:id/publish · /unpublish | 上架 / 下架 |
| POST | /api/admin/products/:id/skus | 新建 SKU |
| PUT / DELETE | /api/admin/skus/:id | 编辑 / 删除 SKU |

### 跨模块端口（service 最小接口，进程内调用）

| 端口 | 实现方消费 | 说明 |
| --- | --- | --- |
| `GetSKU` | cart、flashsale、order | SKU 存在性校验 |
| `GetDetail` | order | 详情仅上架可见（404 即下架） |
| `GetProduct` | cart、flashsale | 直读商品事实；cart 用于绕过详情缓存判断可售性，flashsale 用于商品标题 |
| `GetSKUForUpdate` | order | 订单事务内锁定 SKU 并校验商品仍上架 |
| `DeductStock` / `RestoreStock` | order | 事务内条件扣减 / 回补库存 |
| `BeginDetailMutation` / `FinishDetailMutation` | order | 包围 MySQL 事务建立 / 解除详情写入围栏 |

## 关键机制

### 详情缓存与失效

- 详情 key `product:detail:{id}`，TTL **5min**；永久代次 key `product:detail-version:{id}`；写入围栏 key `product:detail-mutation:{id}` 为按过期时间保存活动 mutation token 的 Redis Sorted Set，每个 token TTL **30min**
- 未命中时先读取代次，直查 DB 后仅在代次未变化且围栏不存在时由 Lua 原子回填；缓存读故障降级直查
- 商品/SKU 变更前，Lua 原子加入独立 token、递增代次并删除详情，紧跟 `WAITAOF` 确认本地 AOF fsync 后才允许数据库写入；回填前清理已过期 token；数据库操作结束后再次递增代次、删除详情并只移除自己的 token，同样经 `WAITAOF` 确认。迟到的旧事务不能解除新围栏，重叠事务也不会延长已失效 token，Redis 重启不会回退已确认围栏；建立失败时拒绝数据库变更；结束失败时围栏保留，详情继续降级直查，不会重新发布旧快照
- 普通订单的库存扣减/回补属于调用方事务：order 以 `BeginDetailMutation` / `FinishDetailMutation` 包围整个 MySQL 事务

### 订单事务内的库存扣减（防超卖）

```text
GetSKUForUpdate(tx, skuID)
  → 非锁定读取 product_id
  → 按 商品 → SKU 的固定顺序 FOR UPDATE 加锁（全局一致锁序，防多 SKU 订单死锁）
  → 校验商品仍 on_sale（下架与下单并发时防售出下架商品）
BeginDetailMutation(productID)
DeductStock(tx, skuID, qty)
  → UPDATE skus SET stock = stock - qty WHERE id = ? AND stock >= qty（条件更新）
  → 未命中时重查区分原因：SKU 已删 → 404；商品下架 → 404；否则库存不足 → 409
RestoreStock(tx, skuID, qty)  ← 取消订单回补
事务结束 → FinishDetailMutation(productID)
```

权威源：[docs/DESIGN.md 普通订单流程](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000003_products` / `000019_order_payment_invariants`。
