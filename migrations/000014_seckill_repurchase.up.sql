-- T13 秒杀超时取消允许再次抢购：原 uk_orders_user_activity 对 (user_id, activity_id)
-- 全状态唯一——取消/超时取消的订单也占去重位，同一用户取消后无法再抢同一活动。
-- 改为"仅非取消订单唯一"：user_activity_key 由应用在状态迁移同事务维护
-- （秒杀落单写 "user_id:activity_id"，取消置 NULL）。MySQL 唯一索引允许多个
-- NULL：取消订单不再占位（允许再次抢购）；非取消订单仍唯一（挡重复落单/并发超卖）。
-- 注意：不用生成列方案——MySQL 限制生成列不得引用外键列（user_id/activity_id 均在 FK 中）。

ALTER TABLE orders
    ADD COLUMN user_activity_key VARCHAR(64) NULL COMMENT '秒杀去重键（落单写 user_id:activity_id；取消同事务置 NULL 允许再次抢购；普通订单恒 NULL）',
    DROP INDEX uk_orders_user_activity,
    ADD UNIQUE KEY uk_orders_user_activity_key (user_activity_key);

-- 存量回填：历史非取消秒杀订单按 (user_id, activity_id) 生成去重键
-- （旧唯一约束保证无重复，回填不冲突；取消订单保持 NULL 不占位）。
UPDATE orders
    SET user_activity_key = CONCAT(CAST(user_id AS CHAR), ':', CAST(activity_id AS CHAR))
    WHERE order_type = 'seckill' AND status <> 'cancelled' AND activity_id IS NOT NULL;
