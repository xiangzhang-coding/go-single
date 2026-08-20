-- Preflight before destructive DDL. A cancelled order followed by a repurchase
-- creates multiple historical rows for one user/activity, which the old schema
-- cannot represent. Fail before dropping user_activity_key instead of leaving a
-- partially downgraded table.
CREATE TEMPORARY TABLE migration_000014_order_pairs (
    user_id BIGINT UNSIGNED NOT NULL,
    activity_id BIGINT UNSIGNED NOT NULL,
    UNIQUE KEY uk_migration_000014_order_pairs (user_id, activity_id)
);

INSERT INTO migration_000014_order_pairs (user_id, activity_id)
SELECT user_id, activity_id
FROM orders
WHERE activity_id IS NOT NULL;

DROP TEMPORARY TABLE migration_000014_order_pairs;

ALTER TABLE orders
    DROP INDEX uk_orders_user_activity_key,
    DROP COLUMN user_activity_key,
    ADD UNIQUE KEY uk_orders_user_activity (user_id, activity_id);
