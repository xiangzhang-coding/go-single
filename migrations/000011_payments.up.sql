-- 支付流水：每次模拟支付尝试（成功/失败）落一条流水。
-- payment_id 客户端生成，UNIQUE 约束挡重复回调（幂等兜底）；服务层另以状态机校验拒绝。
-- result: success(成功) / fail(失败)；失败流水留档审计，订单停留待支付可重付。
-- amount 为支付回调申报金额（分），成功回调与订单 pay_amount 核对（条件更新 WHERE 兜底）。

CREATE TABLE IF NOT EXISTS payments (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    payment_id VARCHAR(64)     NOT NULL COMMENT '支付流水号（客户端生成）',
    order_no   VARCHAR(20)     NOT NULL,
    user_id    BIGINT UNSIGNED NOT NULL,
    amount     BIGINT UNSIGNED NOT NULL COMMENT '回调申报金额，单位：分（与订单应付核对）',
    result     VARCHAR(16)     NOT NULL COMMENT 'success: 支付成功 / fail: 支付失败',
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_payments_payment_id (payment_id),
    KEY idx_payments_order (order_no),
    KEY idx_payments_user (user_id),
    CONSTRAINT fk_payments_order FOREIGN KEY (order_no) REFERENCES orders (order_no) ON DELETE RESTRICT,
    CONSTRAINT fk_payments_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
