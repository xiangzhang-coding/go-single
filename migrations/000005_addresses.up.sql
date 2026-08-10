-- 地址簿：用户维护的收货地址集合。
-- 默认地址唯一性由 users.default_address_id 指针保证（一列只能指向一条地址）；
-- 删除默认地址时 FK ON DELETE SET NULL 自动解除指向（避免双份状态）。

CREATE TABLE IF NOT EXISTS user_addresses (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id    BIGINT UNSIGNED NOT NULL,
    receiver   VARCHAR(64)     NOT NULL COMMENT '收货人',
    phone      VARCHAR(20)     NOT NULL COMMENT '手机号',
    province   VARCHAR(64)     NOT NULL COMMENT '省',
    city       VARCHAR(64)     NOT NULL COMMENT '市',
    district   VARCHAR(64)     NOT NULL COMMENT '区/县',
    detail     VARCHAR(255)    NOT NULL COMMENT '详细地址',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_user_addresses_user (user_id),
    CONSTRAINT fk_user_addresses_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- 默认地址指针：与 user_addresses 构成环状外键，需在两张表都存在后以 ALTER 添加。
ALTER TABLE users
    ADD COLUMN default_address_id BIGINT UNSIGNED NULL COMMENT '默认地址（指向 user_addresses.id，唯一性由此列保证）' AFTER role,
    ADD CONSTRAINT fk_users_default_address FOREIGN KEY (default_address_id) REFERENCES user_addresses (id) ON DELETE SET NULL;
