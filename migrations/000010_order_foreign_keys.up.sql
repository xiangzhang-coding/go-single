-- 订单可选关联的完整性约束：普通订单券、秒杀活动均必须引用已存在记录。
-- 订单表在 000009 中先创建，单独迁移便于已有环境平滑升级。

ALTER TABLE orders
    ADD CONSTRAINT fk_orders_activity
        FOREIGN KEY (activity_id) REFERENCES flashsale_activities (id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_orders_coupon
        FOREIGN KEY (coupon_id) REFERENCES user_coupons (id) ON DELETE RESTRICT;
