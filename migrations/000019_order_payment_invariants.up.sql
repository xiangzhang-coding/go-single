-- R07 makes normal-order request identity, monetary relationships, and price
-- limits database-enforced facts. Legacy normal orders keep a NULL request ID
-- because their original client_request_id cannot be reconstructed.
ALTER TABLE orders
    ADD COLUMN client_request_id VARBINARY(64) NULL AFTER user_id,
    ADD UNIQUE KEY uk_orders_user_client_request (user_id, client_request_id),
    ADD CONSTRAINT chk_orders_amount_range CHECK (
        total_amount <= 9223372036854775807 AND
        discount_amount <= 9223372036854775807 AND
        pay_amount <= 9223372036854775807
    ),
    ADD CONSTRAINT chk_orders_amount_relation CHECK (
        discount_amount <= total_amount AND
        CAST(total_amount AS DECIMAL(20, 0)) =
            CAST(discount_amount AS DECIMAL(20, 0)) + CAST(pay_amount AS DECIMAL(20, 0))
    );

ALTER TABLE skus
    ADD CONSTRAINT chk_skus_price_max CHECK (price <= 100000000);

ALTER TABLE flashsale_activities
    ADD CONSTRAINT chk_flashsale_activities_price_max CHECK (price BETWEEN 1 AND 100000000);

ALTER TABLE flashsale_pre_deductions
    ADD CONSTRAINT chk_flashsale_pre_deductions_price_max CHECK (price IS NULL OR price BETWEEN 1 AND 100000000);

ALTER TABLE order_items
    ADD CONSTRAINT chk_order_items_price_max CHECK (price <= 100000000),
    ADD CONSTRAINT chk_order_items_quantity CHECK (quantity BETWEEN 1 AND 99),
    ADD CONSTRAINT chk_order_items_subtotal CHECK (
        CAST(subtotal AS DECIMAL(20, 0)) =
            CAST(price AS DECIMAL(20, 0)) * CAST(quantity AS DECIMAL(20, 0))
    );
