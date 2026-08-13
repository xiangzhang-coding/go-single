---
sidebar_position: 2
---

# 演示账号

## 管理员

| 项 | 值 |
| --- | --- |
| 用户名 | `admin` |
| 密码 | `admin123` |
| 角色 | admin（后台管理） |

由迁移 `migrations/000002_users.up.sql` 种入（bcrypt 哈希），首次启动后端后即可登录。

登录后侧边栏/导航进入**后台管理**（`/admin`），可运营：

- 商品管理：类目 / 商品 / SKU 与库存
- 订单管理：查看全量订单、对待支付订单发货
- 秒杀活动管理：创建 / 编辑 / 上架 / 下架（时间窗口、独立库存、秒杀价、每人限购）
- 券模板管理：发布直减 / 满减券（总量、每人限领、有效期）

## 普通用户

无预置普通账号——注册一个即可（`/register`，用户名 + 密码，bcrypt 加密存储）。

:::tip 体验双人功能
好友申请、聊天需要两个账号，建议注册两个用户（如 `alice` / `bob`）互相加好友，再体验好友圈与聊天。
:::

## JWT 说明

登录成功返回 JWT（有效期 2h，见 `configs/config.yaml` 的 `auth.ttl`），前端存于 localStorage，经 `Authorization` 头随请求携带；过期后重新登录即可（refresh token 进 backlog）。
