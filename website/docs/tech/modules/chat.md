---
sidebar_position: 10
---

# chat — 即时通信

**定位**：会话 / 消息（text / image / file，图片与文件经 MinIO 存储）、`client_request_id` 幂等、游标分页、已读推进；落库成功后经 WebSocket 实时推送，断线由落库 + REST 拉取兜底；仅好友可单聊。

实现：`internal/chat/`（REST 通道）+ `internal/platform/ws`（实时通道）。

## 数据模型

### conversations

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| conversation_key | VARCHAR(64) PK | `min(uidA,uidB):max(uidA,uidB)` 有序用户对——同一对用户唯一一个会话，与谁先发消息无关 |
| user_a / user_b | BIGINT UNSIGNED FK | 较小 / 较大用户 id |
| last_message_id | BIGINT UNSIGNED | 最近消息 id（会话列表排序与预览） |

### messages

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | BIGINT UNSIGNED PK | 消息 ID（游标） |
| conversation_key | VARCHAR(64) FK | 所属会话（CASCADE） |
| sender_id / recipient_id | BIGINT UNSIGNED FK | 发送方 / 接收方 |
| type | VARCHAR(16) | `text`（用 content）/ `image` / `file`（用 url 托管引用） |
| content | VARCHAR(2000) | text 内容（1–2000 字符） |
| url | VARCHAR(500) | image/file 托管引用（发送者上传、类型匹配，1–500 字符） |
| client_request_id | VARCHAR(64) NULL | 幂等键（可空；NULL 不参与唯一约束，非幂等发送多次落库多行） |

**UNIQUE (sender_id, client_request_id)**：同一发送方同一幂等键仅一条消息（重放返回原消息）。

### conversation_reads

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| user_id + conversation_key | 复合 PK | 读者 + 会话 |
| last_read_message_id | BIGINT UNSIGNED | 已读游标（**只进不退**） |

## 接口

### HTTP（handler/message_handler.go，全部 Bearer）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | /api/messages | 发送消息 `{to_user_id, type, content?, url?, client_request_id?}`；首次 201 / 幂等重放 200（同 id） |
| GET | /api/conversations | 我的会话列表（`before_id` 游标 + `limit`，默认 20 上限 50；最近消息 + 未读数 + 对方用户名） |
| GET | /api/conversations/:key/messages | 会话消息（`after_id` 拉新 / `before_id` 拉旧，互斥；均缺省取最近 limit 条；返回正序 + has_more） |
| POST | /api/conversations/:key/read | 推进已读游标 `{last_message_id}`（只进不退） |

### 实时通道（platform/ws，`GET /ws`）

- 握手：`GET /ws`，浏览器以子协议列表 `bearer, <jwt>` 携带 JWT，凭据不进入 URL；鉴权失败升级前 401，连接配额超限返回 429 + `scope`
- 事件：`{"event":"new_message","data":{Message}}`——消息落库成功后推送给**在线接收方**；离线为无操作（落库 + 上线 REST 补拉兜底）
- 生命周期：JWT 到期时服务端以 4001 主动关闭，前端停止使用旧凭据重连并回到登录；总连接、单用户和单来源 IP 上限均可配置
- 心跳：Ping 间隔 `ws.heartbeat_interval`（默认 30s），pong_wait = 2× 间隔判定断线；写超时 `ws.write_wait`（默认 10s）；每连接发送缓冲 64 条，**慢消费者关闭连接**（客户端重连后 REST 补拉）

## 关键流程

### 发送消息

```text
POST /api/messages {to_user_id, type, ...}
  → 参数校验（text 必填 content；image/file 必填发送者拥有且类型匹配的托管引用；自聊 400）
  → 接收方存在校验（user.GetByID，跨模块）
  → 好友关系校验（social.AreFriends，仅好友可单聊；非好友 403）
  → 单事务：Ensure 会话（不存在则建）→ 消息落库 → Touch 会话最近消息
  → 撞唯一键 (sender_id, client_request_id) → 回滚后查既有消息返回（幂等重放，不重复推送）
  → 落库成功（首次）→ MessageNotifier.NotifyMessageSent → WS Hub 向在线接收方推送
      （实现为 cmd/server 的 wsMessageNotifier 适配器；非阻塞投递）
GET /api/files/:reference
  → 仅引用该媒体消息的发送方与接收方可读取/下载，第三人 403
```

### 会话与消息查询

```text
ListConversations：会话行（last_message_id 倒序，limit+1 探更多）
  → 批量补对方用户名 / 最近消息预览 / 未读数（各一次查询）
ListMessages：after_id（id > cursor，正序）| before_id（id < cursor）
  | 缺省取最近 limit 条；旧消息方向倒序 → 反转成正序返回
MarkRead：会话可达（404/403）→ 消息存在且属于该会话 → 游标只进不退
```

跨模块端口：依赖 `user.GetByID`、`social.AreFriends`；`MessageNotifier` 为业务对平台基础设施端口（实现：platform/ws Hub）。

权威源：[docs/DESIGN.md 即时通信](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、迁移 `000013_messages`。
