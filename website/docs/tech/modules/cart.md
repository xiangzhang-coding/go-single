---
sidebar_position: 4
---

# cart — 购物车

**定位**：暂存待购条目（引用 SKU），数量调整 / 删除；列表拼装展示快照（跨模块经 product 服务接口）。

实现：`internal/cart/`。

## 数据模型

### cart_items

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 条目 ID |
| user_id | BIGINT UNSIGNED FK | 归属用户（CASCADE） |
| sku_id | BIGINT UNSIGNED FK | 引用 SKU（CASCADE：SKU 删除自动清条目） |
| quantity | INT UNSIGNED | 数量（1–99） |

**UNIQUE (user_id, sku_id)**：重复加购同一 SKU 合并数量（服务层先查后并；并发由唯一键仲裁后重查再合并）。

:::note
`skus` 被 `order_items`、`posts`、`flashsale_activities` 以 FK RESTRICT 引用——有订单/动态/活动历史的 SKU 实际不可删除，CASCADE 清条目仅对从未被引用的 SKU 可达（见 [product](./product)）。
:::

### CartItemView（列表读模型）

条目 + SKU/商品只读快照：`product_id` / `title` / `specs` / `price` / `stock`（仓储跨表拼装，不含商品域写路径）。

## 接口

### HTTP（handler/cart_handler.go，全部 Bearer）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api/cart | 我的购物车列表（含 SKU/商品展示快照，**新加购的排最前**） |
| POST | /api/cart | 加购 `{sku_id, quantity}`（重复加购合并数量，上限 99） |
| PUT | /api/cart/items/:id | 修改数量 `{quantity}`（owner 校验，防 IDOR） |
| DELETE | /api/cart/items/:id | 删除条目（owner 校验） |

### 跨模块端口（供 order 结算事务）

| 端口 | 说明 |
| --- | --- |
| `LockItems(ctx, tx, userID)` | 订单事务内锁定并读取当前全部条目（FOR UPDATE） |
| `DeletePurchased(ctx, tx, userID, itemIDs)` | 按锁定的条目 ID 删除已购行（避免按 SKU 误删并发变更） |

## 关键流程

### 加购

```text
POST /api/cart {sku_id, quantity}
  → 数量校验（1–99）
  → product.GetSKU（不存在 → 404）
  → product.GetDetail（仅上架可见，404 即商品下架 → 409）
  → 已存在 (user, sku) 条目？合并数量（上限 99）: 新建
  （并发撞唯一键 → 重查后走合并路径）
```

错误映射：SKU 不存在 404 / 商品下架 409 / 条目不存在 404 / 条目归属他人 403。

权威源：[docs/DESIGN.md 模块依赖 DAG](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)（cart → product）、迁移 `000007_cart_items`。
