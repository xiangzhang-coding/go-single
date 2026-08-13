---
sidebar_position: 1
---

# 技术文档 · 模块

镜像 `internal/` 的模块视图：每模块一节（数据模型 / 接口 / 时序）。当前为骨架占位，详细规格随各模块实现同步产出（见 [BACKLOG 建议实现顺序](https://github.com/xiangzhang-coding/go-single/blob/main/docs/BACKLOG.md)）。

| 模块 | 定位 | 领域 |
| --- | --- | --- |
| [user](./user) | 注册/登录/鉴权 + 地址簿 | 基础 |
| [product](./product) | 类目 / 商品（SPU）/ SKU 与库存 | [商品域](../domains/merchandise) |
| [cart](./cart) | 购物车（条目引用 SKU） | [商品域](../domains/merchandise) |
| [order](./order) | 下单/订单状态机/超时取消 | [交易域](../domains/trade) |
| [flashsale](./flashsale) | 秒杀活动/预扣/异步落单/对账 | [交易域](../domains/trade) + [运营域](../domains/operations) |
| [payment](./payment) | 模拟支付回调驱动状态流转 | [交易域](../domains/trade) |
| [coupon](./coupon) | 券模板/领券防超发/核销与回退 | [交易域](../domains/trade) |
| [social](./social) | 好友关系 + 好友圈动态 | [社交域](../domains/social) |
| [chat](./chat) | 会话/消息/实时推送（WS） | [通信域](../domains/communication) |
| [platform](./platform) | 共享基础设施（config/logger/metrics/auth/limiter/cors/cron/mq/cache/ws/file） | 基础 |

## 模块化单体约定

- 模块间经 **service 接口进程内调用**（面向接口，非 HTTP），依赖方向无环
- admin 管理入口按模块内嵌（product/order/flashsale/coupon 的 admin 路由 + role 鉴权），不单独成模块
- 权威源：`docs/DESIGN.md` 的[整体结构](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)与各模块实现（`internal/`）；本目录只放摘要与链接
