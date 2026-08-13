---
sidebar_position: 10
---

# chat — 即时通信

**定位**：会话 / 消息（text / image / file，图片与文件经 MinIO 存储）、`client_request_id` 幂等、游标分页、已读推进；落库成功后经 WebSocket 实时推送，断线由落库 + REST 拉取兜底；仅好友可单聊。

**状态**：占位。数据模型 / 接口 / 时序待后续填充（实现：`internal/chat/`）。

领域：见[通信域](../domains/communication)。
