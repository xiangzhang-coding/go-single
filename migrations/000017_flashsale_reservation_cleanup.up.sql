ALTER TABLE flashsale_pre_deductions
    ADD COLUMN reservation_released_at DATETIME(3) NULL COMMENT 'ordered 后已无需回退时清理 Redis reservation marker' AFTER rolled_back_at;
