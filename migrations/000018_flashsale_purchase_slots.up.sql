-- R05 freezes the accepted flash-sale deal and scopes idempotency to one
-- purchase slot. Existing R04 rows are backfilled from their activity as the
-- best available deployment snapshot; in-flight messages then replay from the
-- durable row instead of rereading mutable activity fields.
ALTER TABLE flashsale_pre_deductions
    ADD COLUMN client_request_id VARCHAR(64) NULL AFTER activity_id,
    ADD COLUMN sku_id BIGINT UNSIGNED NULL AFTER order_no,
    ADD COLUMN price BIGINT UNSIGNED NULL AFTER sku_id,
    ADD COLUMN purchase_slot BIGINT UNSIGNED NULL AFTER quantity;

UPDATE flashsale_pre_deductions AS pd
JOIN flashsale_activities AS a ON a.id = pd.activity_id
SET pd.client_request_id = CONCAT('migrated-r05-', CAST(pd.id AS CHAR)),
    pd.sku_id = a.sku_id,
    pd.price = a.price,
    pd.purchase_slot = pd.id
WHERE pd.client_request_id IS NULL;

ALTER TABLE flashsale_pre_deductions
    ADD UNIQUE KEY uk_flashsale_pre_deductions_request (user_id, activity_id, client_request_id),
    ADD KEY idx_flashsale_pre_deductions_sku (sku_id),
    ADD CONSTRAINT fk_flashsale_pre_deductions_sku FOREIGN KEY (sku_id) REFERENCES skus (id) ON DELETE RESTRICT;

ALTER TABLE orders
    ADD COLUMN purchase_slot BIGINT UNSIGNED NULL AFTER activity_id,
    ADD KEY idx_orders_activity_slot (activity_id, purchase_slot);

UPDATE orders AS o
LEFT JOIN flashsale_pre_deductions AS pd ON pd.order_no = o.order_no
SET o.purchase_slot = COALESCE(pd.purchase_slot, 1)
WHERE o.order_type = 'seckill' AND o.activity_id IS NOT NULL;

UPDATE orders
SET user_activity_key = CONCAT(
    CAST(user_id AS CHAR), ':', CAST(activity_id AS CHAR), ':', CAST(purchase_slot AS CHAR)
)
WHERE order_type = 'seckill' AND status <> 'cancelled' AND activity_id IS NOT NULL;
