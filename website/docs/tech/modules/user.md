---
sidebar_position: 2
---

# user — 用户与认证

**定位**：注册（bcrypt 加密）/ 登录（JWT，2h）/ 个人资料（昵称/头像）/ 用户搜索 / 地址簿（默认地址唯一）。

实现：`internal/user/`（handler / service / repository / model）。

## 数据模型

### users

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 用户 ID |
| username | VARCHAR(32) UNIQUE | 用户名（3–32 字符） |
| nickname | VARCHAR(64) NULL | 昵称（可空，≤32 字符；空时前端回退展示 username） |
| avatar_url | VARCHAR(255) NULL | 头像托管引用（可空；`POST /api/files` 上传返回） |
| password_hash | VARCHAR(255) | bcrypt 哈希（默认 cost=10），日志不记录密码 |
| role | VARCHAR(16) | `user` / `admin` |
| default_address_id | BIGINT UNSIGNED NULL | **默认地址唯一性指针**（FK → user_addresses.id ON DELETE SET NULL） |
| created_at / updated_at | DATETIME(3) | — |

admin 种子账号 `admin/admin123` 由迁移种入（见[演示账号](../../user-guide/demo-accounts)）。

头像采用与图片消息/动态配图一致的托管引用模式：前端先 `POST /api/files` 上传到 MinIO 私有桶，取回 `/files/<opaque-ref>`，再经 `PATCH /api/users/me` 写入 `avatar_url`。user 服务通过最小媒体端口校验对象真实存在、类型为 image 且归当前用户；任意外部 URL 和他人引用均被拒。展示时前端经带 Bearer 的 `GET /api/files/:reference` 拉取 Blob，不依赖桶匿名访问。

### user_addresses（地址簿）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 地址 ID |
| user_id | BIGINT UNSIGNED FK | 归属用户（CASCADE） |
| receiver / phone / province / city / district / detail | VARCHAR | 收货信息；手机号校验 `^1[3-9]\d{9}$` |
| is_default | 派生标记 | **不落库**：读取时由 `users.default_address_id` 指针推导 |

设计要点：默认地址唯一性由 `users.default_address_id` 单指针保证（一列只能指向一条地址），避免"多条 is_default=true"的双份状态；删除默认地址时 FK `ON DELETE SET NULL` 自动解除指向，随后由服务层自愈——仍有余下地址时把最新一条提为默认。

## 接口

### HTTP（handler/user_handler.go + address_handler.go）

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | /api/auth/register | 无 | 注册 `{username, password}`；用户名重复 409 |
| POST | /api/auth/login | 无 | 登录，返回 `{token, user}`；凭证错误 401 |
| GET | /api/users/me | Bearer | 当前用户（含 nickname/avatar_url） |
| PATCH | /api/users/me | Bearer | 修改个人资料 `{nickname?, avatar_url?}`（PATCH 语义：未提交字段不动、空串清空；昵称 ≤32 字符；avatar_url 非空时必须是本人上传的托管图片；归属由 token 声明保证） |
| GET | /api/users | Bearer | 按用户名前缀搜索（`username` + `limit`，默认 10 上限 20；**排除自己**——"加好友"发现入口） |
| GET | /api/users/:id | Bearer | 指定用户（仅本人或 admin，防 IDOR） |
| GET | /api/addresses | Bearer | 我的地址列表（默认地址排最前） |
| POST | /api/addresses | Bearer | 新增地址（**首条自动设为默认**；`is_default=true` 显式设默认） |
| PUT | /api/addresses/:id | Bearer | 编辑地址（不触碰默认指向） |
| DELETE | /api/addresses/:id | Bearer | 删除地址（删默认地址 → 最新余下地址提为默认） |
| PUT | /api/addresses/:id/default | Bearer | 设为默认（单条 UPDATE 切换指针，旧默认自动失效） |

### 跨模块端口（service 最小接口，进程内调用）

| 端口 | 实现方消费 | 说明 |
| --- | --- | --- |
| `GetByID` | social（补用户名）、chat（接收方校验） | 用户查询 |
| `GetAddress` | order | 下单固化地址快照（owner 校验） |
| `GetDefaultAddress` | flashsale 落单消费者 | 秒杀订单地址快照（无默认地址 → 永久失败进死信） |

## 关键流程

### 登录

```text
POST /api/auth/login
  → 按 username 查用户（不存在 → 401）
  → bcrypt.CompareHashAndPassword（不匹配 → 401）
  → JWT 签发（HS256，TTL 2h，sub=user_id，role 声明）
  → 返回 {token, user}
```

鉴权中间件：`Authorization: Bearer <token>` → `TokenVerifier.Verify`（失败 401）→ Claims 写入上下文；admin 路由另加 `RequireAdmin`（非 admin 403）。

权威源：[docs/DESIGN.md 认证与权限](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000001_init` / `000002_users` / `000005_addresses` / `000015_user_profile`。
