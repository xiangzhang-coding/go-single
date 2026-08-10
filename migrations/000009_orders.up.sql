-- 订单与订单项（普通/秒杀共用状态机）。
-- 状态：pending_payment(待支付) → paid(已支付) → shipped(已发货) → completed(已完成)；
--       含 cancelled(已取消) 与超时取消；非法跃迁由服务层状态机拒绝，更新用条件更新兜底。
-- 订单号：雪花 ID（手写实现，VARCHAR 主键；JSON 按字符串序列化避免 JS 精度丢失）。
-- 金额：total_amount 商品总额 − discount_amount 券额 = pay_amount 应付（单位：分）。
-- 地址快照：下单时从地址簿固化为副本，用户后续改地址不影响历史订单。
-- expire_at：超时取消时间（普通 15min；秒杀 10min 由秒杀落单路径写入）。

CREATE TABLE IF NOT EXISTS orders (
    order_no       VARCHAR(20)     NOT NULL COMMENT '雪花订单号',
    user_id        BIGINT UNSIGNED NOT NULL,
    order_type     VARCHAR(16)     NOT NULL DEFAULT 'normal' COMMENT 'normal: 普通 / seckill: 秒杀',
    status         VARCHAR(16)     NOT NULL DEFAULT 'pending_payment',
    activity_id    BIGINT UNSIGNED NULL COMMENT '秒杀活动（秒杀订单专属；(user_id, activity_id) 唯一）',
    total_amount   BIGINT UNSIGNED NOT NULL COMMENT '商品总额，单位：分',
    discount_amount BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '券额，单位：分',
    pay_amount     BIGINT UNSIGNED NOT NULL COMMENT '应付 = 总额 - 券额，单位：分',
    coupon_id      BIGINT UNSIGNED NULL COMMENT '核销的用户券（秒杀订单不使用）',
    receiver       VARCHAR(64)     NOT NULL COMMENT '收货人（地址快照）',
    phone          VARCHAR(20)     NOT NULL COMMENT '手机号（地址快照）',
    province       VARCHAR(64)     NOT NULL COMMENT '省（地址快照）',
    city           VARCHAR(64)     NOT NULL COMMENT '市（地址快照）',
    district       VARCHAR(64)     NOT NULL COMMENT '区/县（地址快照）',
    detail         VARCHAR(255)    NOT NULL COMMENT '详细地址（地址快照）',
    paid_at        DATETIME(3)     NULL,
    shipped_at     DATETIME(3)     NULL,
    completed_at   DATETIME(3)     NULL,
    cancelled_at   DATETIME(3)     NULL,
    expire_at      DATETIME(3)     NOT NULL COMMENT '超时取消时间（普通 15min）',
    created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (order_no),
    KEY idx_orders_user_status (user_id, status),
    UNIQUE KEY uk_orders_user_activity (user_id, activity_id),
    CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- 订单项：下单时固化的商品快照（标题/规格/单价），价格不受后续改价影响。
-- sku_id RESTRICT：有订单历史的 SKU 不可删除，保证对账与历史可追溯。
CREATE TABLE IF NOT EXISTS order_items (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    order_no   VARCHAR(20)     NOT NULL,
    sku_id     BIGINT UNSIGNED NOT NULL,
    product_id BIGINT UNSIGNED NOT NULL,
    title      VARCHAR(128)    NOT NULL COMMENT '商品标题快照',
    specs      VARCHAR(255)    NOT NULL COMMENT '规格组合快照',
    price      BIGINT UNSIGNED NOT NULL COMMENT '成交单价快照，单位：分',
    quantity   INT UNSIGNED    NOT NULL,
    subtotal   BIGINT UNSIGNED NOT NULL COMMENT '小计 = price * quantity',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_order_items_order (order_no),
    KEY idx_order_items_sku (sku_id),
    CONSTRAINT fk_order_items_order FOREIGN KEY (order_no) REFERENCES orders (order_no) ON DELETE CASCADE,
    CONSTRAINT fk_order_items_sku FOREIGN KEY (sku_id) REFERENCES skus (id) ON DELETE RESTRICT
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
