---
sidebar_position: 9
---

# social — 好友与好友圈

**定位**：好友申请 / 通过 / 拒绝与好友列表；好友圈动态（引用已购 SKU + 文案 + 图片，购买校验经 order 服务）、时间线（仅好友可见，拉取式分页）、删除动态。

实现：`internal/social/`（friend_service.go + post_service.go）。

## 数据模型

### friend_requests（好友申请）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 申请 ID |
| from_user_id / to_user_id | BIGINT UNSIGNED FK | 申请人 / 被申请人（CASCADE） |
| status | VARCHAR(16) | `pending`（待处理）→ `accepted`（通过）/ `rejected`（拒绝） |

**UNIQUE (from_user_id, to_user_id)**：每对唯一一行——并发首提由唯一键仲裁（服务层冲突兜底分流）；**被拒后可重新申请**（复用原行条件更新回 pending，保留历史）。状态由 CHECK 限定为三种合法值，收件/发件分页均有覆盖索引。

### friend_pair_locks（用户对事务锁）

`PRIMARY KEY (user_a, user_b)`，其中 `user_a < user_b`。发起、通过和拒绝以单条 `INSERT ... ON DUPLICATE KEY UPDATE` 对精确用户对直接获取排他锁并保持到事务结束，串行同一用户对的状态操作；不同用户对使用不同主键，不锁不存在申请索引的间隙，也不发生共享锁升级。

### friendships（好友关系）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 关系行 ID |
| user_id / friend_id | BIGINT UNSIGNED FK | 用户 / 好友（CASCADE；CHECK 禁止自加） |

一对好友存**方向相反的两行**（(A,B) 与 (B,A) 同时写入）：好友列表 `WHERE user_id = ?` 双向可查，状态总对称。

### posts（好友圈动态）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 动态 ID |
| user_id | BIGINT UNSIGNED FK | 发布者（CASCADE） |
| sku_id | BIGINT UNSIGNED FK | 引用已购 SKU（**RESTRICT**：有动态引用的 SKU 不可删除） |
| content | VARCHAR(500) | 可选文案（空串 = 未填） |
| image_url | VARCHAR(500) | 可选托管图片引用（必须由发布者上传且类型为 image；空串 = 未填） |

## 接口

### HTTP（handler/friend_handler.go + post_handler.go，全部 Bearer）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | /api/friend-requests | 发起申请 `{to_user_id}`；自加 400、目标不存在 404、已好友/已有待处理 409 |
| GET | /api/friend-requests | 我的申请（`scope`=incoming/outgoing，`status` 筛选，`page/page_size` 默认 1/20、每页上限 50；返回 `{items,total}`） |
| POST | /api/friend-requests/:id/accept | 通过申请（**仅被申请人**；建立双向好友关系） |
| POST | /api/friend-requests/:id/reject | 拒绝申请（仅被申请人，且须待处理） |
| GET | /api/friends | 我的好友列表（双向） |
| POST | /api/posts | 分享动态 `{sku_id, content?, image_url?}`；**未购买 403** |
| GET | /api/posts/feed | 好友圈时间线（**仅好友动态**，不含自己；page/page_size 分页） |
| GET | /api/posts/mine | 我的动态（时间倒序分页） |
| DELETE | /api/posts/:id | 删除自己的动态（owner 校验，RowsAffected 兜底防谎报） |

### 跨模块端口

**依赖**：`user.GetByID`（目标存在校验）、`user.GetPublicByIDs`（单次批量补对方/作者用户名）、`order.HasPurchasedSKU`（分享购买校验：已支付/已发货/已完成订单含该 SKU）。**对外**：`AreFriends`（chat 校验"仅好友可单聊"）。

## 关键流程

### 好友建立

```text
POST /api/friend-requests {to_user_id}
  → 自加拒绝 → 目标用户存在校验 → 单事务按稳定顺序锁定双向申请键
  → 已是好友直接 409；否则已有申请行按状态分流：
      pending → 409 重复申请 / accepted → 409 已是好友 / rejected → 复用原行重新申请
POST /api/friend-requests/:id/accept（被申请人）
  → 单事务按稳定顺序锁定同一用户对的双向申请 → owner 校验（403）→ pending 条件迁移（并发落败者 409）
  → 双向建关系（同一事务）→ 申请置 accepted
  → 反向 pending/rejected 申请一并收敛为 accepted，避免"已拒绝但已成好友"
  → 已是好友（含并发撞唯一键）→ 收敛为通过（自愈历史残留）
```

### 好友圈分享与时间线

```text
POST /api/posts
  → 参数校验（content ≤500 字符；image_url 非空时校验系统托管、发布者归属与 image 类型）
  → order.HasPurchasedSKU（未购买 → 403）→ 落库
GET /api/posts/feed
  → 好友列表 → posts 按时间倒序分页（friend_ids IN (...)）→ 批量补作者用户名
GET /api/files/:reference
  → 上传者可读；其他用户仅在当前与动态作者为好友且动态仍存在时可读
```

权威源：[docs/DESIGN.md 社交](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000006_friendships` / `000012_posts` / `000021_friend_request_hardening` / `000022_friend_pair_locks`。
