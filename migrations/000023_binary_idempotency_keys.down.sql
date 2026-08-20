-- Preflight every old unique key before any auto-committing ALTER. If binary-
-- distinct identifiers would collide under utf8mb4_unicode_ci, the INSERT
-- fails and leaves the current schema untouched.
CREATE TEMPORARY TABLE migration_000023_payment_ids (
    payment_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    UNIQUE KEY uk_migration_000023_payment_ids (payment_id)
);
INSERT INTO migration_000023_payment_ids (payment_id)
SELECT CONVERT(payment_id USING utf8mb4) FROM payments;

CREATE TEMPORARY TABLE migration_000023_message_ids (
    sender_id BIGINT UNSIGNED NOT NULL,
    client_request_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    UNIQUE KEY uk_migration_000023_message_ids (sender_id, client_request_id)
);
INSERT INTO migration_000023_message_ids (sender_id, client_request_id)
SELECT sender_id, CONVERT(client_request_id USING utf8mb4)
FROM messages
WHERE client_request_id IS NOT NULL;

CREATE TEMPORARY TABLE migration_000023_flashsale_ids (
    user_id BIGINT UNSIGNED NOT NULL,
    activity_id BIGINT UNSIGNED NOT NULL,
    client_request_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    UNIQUE KEY uk_migration_000023_flashsale_ids (user_id, activity_id, client_request_id)
);
INSERT INTO migration_000023_flashsale_ids (user_id, activity_id, client_request_id)
SELECT user_id, activity_id, CONVERT(client_request_id USING utf8mb4)
FROM flashsale_pre_deductions
WHERE client_request_id IS NOT NULL;

DROP TEMPORARY TABLE migration_000023_flashsale_ids;
DROP TEMPORARY TABLE migration_000023_message_ids;
DROP TEMPORARY TABLE migration_000023_payment_ids;

ALTER TABLE payments
    MODIFY COLUMN payment_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '支付流水号（客户端生成）';

ALTER TABLE messages
    MODIFY COLUMN client_request_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NULL COMMENT '幂等键（可空）';

ALTER TABLE flashsale_pre_deductions
    MODIFY COLUMN client_request_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NULL;
