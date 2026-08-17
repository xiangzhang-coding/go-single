-- R04 durable local saga/outbox for each successful Redis pre-deduction.
CREATE TABLE IF NOT EXISTS flashsale_pre_deductions (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id           BIGINT UNSIGNED NOT NULL,
    activity_id       BIGINT UNSIGNED NOT NULL,
    order_no          VARCHAR(64) NULL,
    quantity          INT UNSIGNED NOT NULL DEFAULT 1,
    status            VARCHAR(32) NOT NULL COMMENT 'preparing/pending_publish/pending_order/ordered/pending_rollback/rolled_back',
    publish_attempts  INT UNSIGNED NOT NULL DEFAULT 0,
    rollback_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL,
    legacy            TINYINT(1) NOT NULL DEFAULT 0 COMMENT 'row adopted from an order created before R04',
    ordered_at        DATETIME(3) NULL,
    rolled_back_at    DATETIME(3) NULL,
    created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_flashsale_pre_deductions_order_no (order_no),
    KEY idx_flashsale_pre_deductions_recovery (status, updated_at, id),
    KEY idx_flashsale_pre_deductions_activity (activity_id, status),
    KEY idx_flashsale_pre_deductions_user (user_id, id),
    CONSTRAINT fk_flashsale_pre_deductions_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT fk_flashsale_pre_deductions_activity FOREIGN KEY (activity_id) REFERENCES flashsale_activities (id) ON DELETE RESTRICT
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
