ALTER TABLE order_items
    DROP CHECK chk_order_items_subtotal,
    DROP CHECK chk_order_items_quantity,
    DROP CHECK chk_order_items_price_max;

ALTER TABLE flashsale_pre_deductions
    DROP CHECK chk_flashsale_pre_deductions_price_max;

ALTER TABLE flashsale_activities
    DROP CHECK chk_flashsale_activities_price_max;

ALTER TABLE skus
    DROP CHECK chk_skus_price_max;

ALTER TABLE orders
    DROP CHECK chk_orders_amount_relation,
    DROP CHECK chk_orders_amount_range,
    DROP INDEX uk_orders_user_client_request,
    DROP COLUMN client_request_id;
