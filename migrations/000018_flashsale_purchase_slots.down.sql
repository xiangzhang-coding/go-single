-- This UPDATE intentionally fails on uk_orders_user_activity_key if multiple
-- active slots now exist for one user/activity. Downgrading must not silently
-- collapse valid R05 orders into the old one-order model.
UPDATE orders
SET user_activity_key = CONCAT(CAST(user_id AS CHAR), ':', CAST(activity_id AS CHAR))
WHERE order_type = 'seckill' AND status <> 'cancelled' AND activity_id IS NOT NULL;

ALTER TABLE orders
    DROP INDEX idx_orders_activity_slot,
    DROP COLUMN purchase_slot;

ALTER TABLE flashsale_pre_deductions
    DROP FOREIGN KEY fk_flashsale_pre_deductions_sku,
    DROP INDEX idx_flashsale_pre_deductions_sku,
    DROP INDEX uk_flashsale_pre_deductions_request,
    DROP COLUMN purchase_slot,
    DROP COLUMN price,
    DROP COLUMN sku_id,
    DROP COLUMN client_request_id;
