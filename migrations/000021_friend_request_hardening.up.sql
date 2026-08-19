ALTER TABLE friend_requests
    ADD CONSTRAINT chk_friend_requests_status
        CHECK (status IN ('pending', 'accepted', 'rejected')),
    ADD KEY idx_friend_requests_incoming_page (to_user_id, id),
    ADD KEY idx_friend_requests_outgoing_page (from_user_id, id),
    ADD KEY idx_friend_requests_outgoing_status_page (from_user_id, status, id);
