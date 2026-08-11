ALTER TABLE orders
    DROP INDEX uk_orders_user_activity_key,
    DROP COLUMN user_activity_key,
    ADD UNIQUE KEY uk_orders_user_activity (user_id, activity_id);
