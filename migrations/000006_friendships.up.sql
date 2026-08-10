-- 好友申请：发起→对方通过/拒绝（待处理→通过/拒绝）。
-- 每对 (from,to) 唯一一行：首提并发由唯一键仲裁（服务层冲突兜底分流）；
-- 被拒后可重新申请（复用原行 UPDATE 回 pending，并发重提幂等，保留历史）。
-- 唯一键同时是"好友间至多一条申请"的兜底保证。

CREATE TABLE IF NOT EXISTS friend_requests (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    from_user_id BIGINT UNSIGNED NOT NULL COMMENT '申请人',
    to_user_id   BIGINT UNSIGNED NOT NULL COMMENT '被申请人',
    status       VARCHAR(16)     NOT NULL DEFAULT 'pending' COMMENT 'pending/accepted/rejected',
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_friend_requests_pair (from_user_id, to_user_id),
    KEY idx_friend_requests_to (to_user_id, status),
    CONSTRAINT fk_friend_requests_from FOREIGN KEY (from_user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_friend_requests_to FOREIGN KEY (to_user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- 好友关系：一对好友存方向相反的两行（user_id ↔ friend_id），
-- 好友列表直接 WHERE user_id = ? 双向可查；CHECK 禁止自加好友。

CREATE TABLE IF NOT EXISTS friendships (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id    BIGINT UNSIGNED NOT NULL COMMENT '用户',
    friend_id  BIGINT UNSIGNED NOT NULL COMMENT '好友',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_friendships_pair (user_id, friend_id),
    CONSTRAINT chk_friendships_not_self CHECK (user_id <> friend_id),
    CONSTRAINT fk_friendships_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_friendships_friend FOREIGN KEY (friend_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
