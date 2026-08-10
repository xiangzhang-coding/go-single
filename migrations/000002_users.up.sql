-- 用户表：注册/登录/角色（user/admin），admin 种子账号由下方 INSERT 种入。
CREATE TABLE IF NOT EXISTS users (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    username      VARCHAR(32)     NOT NULL,
    password_hash VARCHAR(255)    NOT NULL,
    role          VARCHAR(16)     NOT NULL DEFAULT 'user',
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_users_username (username)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- admin 种子账号：admin/admin123（bcrypt 哈希，默认 cost=10）。
INSERT INTO users (username, password_hash, role)
VALUES ('admin', '$2a$10$YDcE3V.LXJpDdAcovEV/D.ZLd2pWN66gelFHvaxI0IHxnCs2yEYRq', 'admin');
