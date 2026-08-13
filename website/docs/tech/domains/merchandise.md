---
sidebar_position: 2
---

# 商品域

**术语**：商品(SPU) / SKU / 购物车（定义见 [CONTEXT.md](https://github.com/xiangzhang-coding/go-single/blob/main/CONTEXT.md)）。

**模块映射**：

| 模块 | 承担 |
| --- | --- |
| [product](../modules/product) | 类目 / 商品(SPU) / SKU 与库存 |
| [cart](../modules/cart) | 购物车（条目引用 SKU） |

**说明**：薄视图页。细节见各模块页与 `internal/product/`、`internal/cart/`。
