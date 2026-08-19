ALTER TABLE friend_requests
    DROP CHECK chk_friend_requests_status,
    DROP INDEX idx_friend_requests_incoming_page,
    DROP INDEX idx_friend_requests_outgoing_page,
    DROP INDEX idx_friend_requests_outgoing_status_page;
