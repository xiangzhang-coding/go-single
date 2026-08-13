---
sidebar_position: 1
---

# 技术文档 · 领域

镜像 `CONTEXT.md` 领域分组的**薄视图**：只做聚合与模块 ↔ 领域映射，细节链接 `tech/modules/`，术语以 CONTEXT.md 为准。

| 领域 | 术语（CONTEXT.md） | 模块 |
| --- | --- | --- |
| [商品域](./merchandise) | 商品(SPU) / SKU / 购物车 | product、cart |
| [交易域](./trade) | 订单 / 地址簿 / 地址快照 / 秒杀活动 / 预扣 / 模拟支付 / 优惠券 | order、flashsale、payment、coupon |
| [社交域](./social) | 好友 / 好友申请 / 好友圈 / 动态 | social |
| [通信域](./communication) | 消息 / 图片消息 / 文件消息 | chat |
| [运营域](./operations) | 对账 | flashsale（对账） |

跨领域支撑：`user`（用户与认证）、`platform`（共享基础设施）、admin 管理入口按模块内嵌。

权威源：仓库根 [CONTEXT.md](https://github.com/xiangzhang-coding/go-single/blob/main/CONTEXT.md)（领域术语总览）与 `docs/DESIGN.md`；本目录只放摘要与链接。
