-- 好友圈动态：购买成功后分享（引用已购 SKU + 可选文案 + 可选图片），
-- 仅好友可见（时间线 = 好友列表 join 本表，服务层拉取式分页）。
-- content / image_url 为空串表示未填；sku_id 经订单域校验确已购买后才允许写入。
-- sku_id RESTRICT：与订单项一致，有动态引用的 SKU 不可删除（历史可追溯）。

CREATE TABLE IF NOT EXISTS posts (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id    BIGINT UNSIGNED NOT NULL COMMENT '发布者',
    sku_id     BIGINT UNSIGNED NOT NULL COMMENT '引用已购 SKU',
    content    VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '可选文案',
    image_url  VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '可选图片（MinIO URL）',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_posts_user (user_id),
    KEY idx_posts_created (created_at, id),
    CONSTRAINT fk_posts_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_posts_sku FOREIGN KEY (sku_id) REFERENCES skus (id) ON DELETE RESTRICT
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
