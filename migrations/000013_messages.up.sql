-- 即时通信（REST 通道）：会话 + 消息 + 已读游标三表。
-- 会话标识 conversation_key = min(uidA,uidB):max(uidA,uidB) 有序用户对；
-- 会话列表按 last_message_id 倒序（最新消息在前），预览取 last_message_id 对应行；
-- 消息幂等键 (sender_id, client_request_id) 唯一（client_request_id 可空：
-- NULL 不参与唯一约束，重复 NULL 互不冲突，即非幂等发送多次落库多行）。

CREATE TABLE IF NOT EXISTS conversations (
    conversation_key VARCHAR(64)  NOT NULL COMMENT 'min(uidA,uidB):max(uidA,uidB)',
    user_a           BIGINT UNSIGNED NOT NULL COMMENT '较小用户 id',
    user_b           BIGINT UNSIGNED NOT NULL COMMENT '较大用户 id',
    last_message_id  BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近消息 id（列表排序与预览）',
    created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (conversation_key),
    KEY idx_conversations_user_a (user_a, last_message_id),
    KEY idx_conversations_user_b (user_b, last_message_id),
    CONSTRAINT fk_conversations_user_a FOREIGN KEY (user_a) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_conversations_user_b FOREIGN KEY (user_b) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS messages (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    conversation_key  VARCHAR(64)     NOT NULL,
    sender_id         BIGINT UNSIGNED NOT NULL COMMENT '发送方',
    recipient_id      BIGINT UNSIGNED NOT NULL COMMENT '接收方',
    type              VARCHAR(16)     NOT NULL COMMENT 'text / image / file',
    content           VARCHAR(2000)   NOT NULL DEFAULT '' COMMENT 'text 内容',
    url               VARCHAR(500)    NOT NULL DEFAULT '' COMMENT 'image/file 内容（MinIO URL）',
    client_request_id VARCHAR(64)     NULL DEFAULT NULL COMMENT '幂等键（可空）',
    created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_messages_idempotent (sender_id, client_request_id),
    KEY idx_messages_conv (conversation_key, id),
    KEY idx_messages_recipient (recipient_id, conversation_key, id),
    CONSTRAINT fk_messages_conversation FOREIGN KEY (conversation_key) REFERENCES conversations (conversation_key) ON DELETE CASCADE,
    CONSTRAINT fk_messages_sender FOREIGN KEY (sender_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_messages_recipient FOREIGN KEY (recipient_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS conversation_reads (
    user_id               BIGINT UNSIGNED NOT NULL COMMENT '读者',
    conversation_key      VARCHAR(64)     NOT NULL,
    last_read_message_id  BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '已读游标（只进不退）',
    updated_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (user_id, conversation_key),
    CONSTRAINT fk_reads_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_reads_conversation FOREIGN KEY (conversation_key) REFERENCES conversations (conversation_key) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
