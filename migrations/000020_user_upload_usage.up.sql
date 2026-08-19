-- Per-user cumulative upload budget. Reservations happen before MinIO writes so
-- repeated legal uploads cannot grow object storage beyond configured limits.
CREATE TABLE IF NOT EXISTS user_upload_usage (
    user_id      BIGINT UNSIGNED NOT NULL,
    used_bytes   BIGINT UNSIGNED NOT NULL DEFAULT 0,
    object_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (user_id),
    CONSTRAINT fk_user_upload_usage_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
