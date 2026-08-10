-- 购物车：登录用户的待购条目集合，条目引用 SKU。
-- (user_id, sku_id) 唯一：重复加购同一 SKU 合并数量（服务层先查后并，
-- 并发兜底由唯一键仲裁后重查合并），SKU 删除经 FK CASCADE 自动清条目。

CREATE TABLE IF NOT EXISTS cart_items (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id    BIGINT UNSIGNED NOT NULL COMMENT '用户',
    sku_id     BIGINT UNSIGNED NOT NULL COMMENT 'SKU',
    quantity   INT UNSIGNED    NOT NULL DEFAULT 1 COMMENT '数量',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_cart_items_user_sku (user_id, sku_id),
    KEY idx_cart_items_user (user_id),
    CONSTRAINT fk_cart_items_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_cart_items_sku FOREIGN KEY (sku_id) REFERENCES skus (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
