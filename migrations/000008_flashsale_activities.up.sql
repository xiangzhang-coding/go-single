-- 秒杀活动（admin 管理）：时间窗口 + 独立库存 + 秒杀价 + 限购 + 手动上下架。
-- 状态约定：status = off_sale(下架) | on_sale(上架)；进行中由时间窗口动态判定，
-- 不显式翻转（DESIGN.md）；status 仅用于手动下架/紧急停止。
-- 库存模型：活动独立库存，与 skus.stock（普通订单库存）互不干扰；落单扣活动库存。

CREATE TABLE IF NOT EXISTS flashsale_activities (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    sku_id         BIGINT UNSIGNED NOT NULL,
    title          VARCHAR(128)    NOT NULL COMMENT '活动标题',
    price          BIGINT UNSIGNED NOT NULL COMMENT '秒杀价，单位：分',
    stock          INT UNSIGNED    NOT NULL COMMENT '活动独立库存',
    per_user_limit INT UNSIGNED    NOT NULL DEFAULT 1 COMMENT '每人限购',
    status         VARCHAR(16)     NOT NULL DEFAULT 'off_sale' COMMENT 'off_sale: 下架 / on_sale: 上架',
    start_at       DATETIME(3)     NOT NULL,
    end_at         DATETIME(3)     NOT NULL,
    created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_flashsale_activities_sku (sku_id),
    CONSTRAINT fk_flashsale_activities_sku FOREIGN KEY (sku_id) REFERENCES skus (id) ON DELETE RESTRICT
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
