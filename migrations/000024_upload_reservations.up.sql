CREATE TABLE user_upload_objects (
    object_key VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    client_request_id VARBINARY(64) NOT NULL,
    size BIGINT UNSIGNED NOT NULL,
    status ENUM('pending', 'committed') NOT NULL DEFAULT 'pending',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (object_key),
    UNIQUE KEY uk_user_upload_objects_request (user_id, client_request_id),
    KEY idx_user_upload_objects_pending (status, created_at),
    KEY idx_user_upload_objects_user (user_id),
    CONSTRAINT fk_user_upload_objects_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
