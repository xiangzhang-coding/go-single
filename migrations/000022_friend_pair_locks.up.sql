CREATE TABLE IF NOT EXISTS friend_pair_locks (
    user_a     BIGINT UNSIGNED NOT NULL COMMENT '较小用户 ID',
    user_b     BIGINT UNSIGNED NOT NULL COMMENT '较大用户 ID',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (user_a, user_b),
    CONSTRAINT chk_friend_pair_locks_order CHECK (user_a < user_b),
    CONSTRAINT fk_friend_pair_locks_user_a FOREIGN KEY (user_a) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_friend_pair_locks_user_b FOREIGN KEY (user_b) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
