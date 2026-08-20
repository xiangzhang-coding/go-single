-- Client-generated idempotency identifiers are opaque text. Binary collation
-- preserves the 64-character contract while preventing case/accent folding.
ALTER TABLE flashsale_pre_deductions
    MODIFY COLUMN client_request_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL;

ALTER TABLE messages
    MODIFY COLUMN client_request_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL COMMENT 'opaque idempotency key (optional)';

ALTER TABLE payments
    MODIFY COLUMN payment_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL COMMENT 'client-generated opaque payment id';
