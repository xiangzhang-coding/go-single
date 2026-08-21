UPDATE user_upload_usage AS u
JOIN (
    SELECT user_id, SUM(size) AS pending_bytes, COUNT(*) AS pending_objects
    FROM user_upload_objects
    WHERE status = 'pending'
    GROUP BY user_id
) AS pending ON pending.user_id = u.user_id
SET u.used_bytes = IF(u.used_bytes >= pending.pending_bytes, u.used_bytes - pending.pending_bytes, 0),
    u.object_count = IF(u.object_count >= pending.pending_objects, u.object_count - pending.pending_objects, 0);

DROP TABLE IF EXISTS user_upload_objects;
