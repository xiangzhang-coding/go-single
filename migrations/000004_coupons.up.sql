-- 优惠券：券模板（admin 发布）+ 用户券（领取，核销见 T07）。
-- 券类型：type = direct(直减) | threshold(满减)；金额单位：分。
-- 券状态：status = unused(未用) | used(已用)；expired(过期) 由读取时按有效期派生，
-- 不落库，避免定时任务与读路径状态漂移。

CREATE TABLE IF NOT EXISTS coupon_templates (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    name           VARCHAR(64)     NOT NULL,
    type           VARCHAR(16)     NOT NULL COMMENT 'direct: 直减 / threshold: 满减',
    value          BIGINT UNSIGNED NOT NULL COMMENT '面额，单位：分',
    min_amount     BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '满减门槛，单位：分；直减为 0',
    total          INT UNSIGNED    NOT NULL COMMENT '发放总量',
    per_user_limit INT UNSIGNED    NOT NULL DEFAULT 1 COMMENT '每人限领',
    valid_from     DATETIME(3)     NOT NULL,
    valid_until    DATETIME(3)     NOT NULL,
    created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS user_coupons (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id     BIGINT UNSIGNED NOT NULL,
    template_id BIGINT UNSIGNED NOT NULL,
    status      VARCHAR(16)     NOT NULL DEFAULT 'unused' COMMENT 'unused: 未用 / used: 已用',
    used_at     DATETIME(3)     NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_user_coupons_user_status (user_id, status),
    KEY idx_user_coupons_template (template_id),
    CONSTRAINT fk_user_coupons_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_user_coupons_template FOREIGN KEY (template_id) REFERENCES coupon_templates (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
