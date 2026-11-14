-- name: CreateFYIMessage :one
INSERT INTO fyi_messages (target_session_id, sender_session_id, message, created_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: CountPendingFYIMessages :one
SELECT COUNT(*) FROM fyi_messages
WHERE target_session_id = ? AND delivery_state = 'pending';

-- name: ClaimPendingFYIMessages :many
UPDATE fyi_messages
SET delivery_state = 'claimed', claim_token = sqlc.arg(claim_token),
    claimed_at = sqlc.arg(claimed_at)
WHERE target_session_id = sqlc.arg(target_session_id) AND delivery_state = 'pending'
RETURNING *;

-- name: AcknowledgeFYIMessages :many
UPDATE fyi_messages
SET delivery_state = 'acknowledged', acknowledged_at = sqlc.arg(acknowledged_at)
WHERE target_session_id = sqlc.arg(target_session_id) AND delivery_state = 'claimed'
  AND claim_token = sqlc.arg(claim_token)
RETURNING id;

-- name: ReleaseFYIMessages :many
UPDATE fyi_messages
SET delivery_state = 'pending', claim_token = '', claimed_at = NULL
WHERE target_session_id = sqlc.arg(target_session_id) AND delivery_state = 'claimed'
  AND claim_token = sqlc.arg(claim_token)
RETURNING id;

-- name: RequeueClaimedFYIMessages :execrows
UPDATE fyi_messages
SET delivery_state = 'pending', claim_token = '', claimed_at = NULL
WHERE delivery_state = 'claimed';
