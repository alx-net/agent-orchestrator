-- +goose Up
-- +goose StatementBegin
-- Informational `ao send --fyi` messages held for a session's next turn. A row
-- never starts a turn on its own: the next turn the target starts carries every
-- pending row in id (arrival) order, and the row is acknowledged only after
-- that turn is accepted. Claim/acknowledge follows the reports outbox.
CREATE TABLE fyi_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    target_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    sender_session_id TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL CHECK (length(trim(message)) > 0 AND length(CAST(message AS BLOB)) <= 4096),
    created_at TIMESTAMP NOT NULL,
    delivery_state TEXT NOT NULL DEFAULT 'pending' CHECK (delivery_state IN ('pending', 'claimed', 'acknowledged')),
    claim_token TEXT NOT NULL DEFAULT '',
    claimed_at TIMESTAMP,
    acknowledged_at TIMESTAMP,
    CHECK (
        (delivery_state = 'pending' AND claim_token = '' AND claimed_at IS NULL AND acknowledged_at IS NULL)
        OR (delivery_state = 'claimed' AND claim_token <> '' AND claimed_at IS NOT NULL AND acknowledged_at IS NULL)
        OR (delivery_state = 'acknowledged' AND claim_token <> '' AND claimed_at IS NOT NULL AND acknowledged_at IS NOT NULL)
    )
);

CREATE INDEX idx_fyi_messages_target_delivery
    ON fyi_messages(target_session_id, delivery_state, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_fyi_messages_target_delivery;
DROP TABLE IF EXISTS fyi_messages;
-- +goose StatementEnd
