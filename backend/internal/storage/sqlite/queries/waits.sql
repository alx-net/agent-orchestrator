-- name: CreateSessionWait :one
INSERT INTO session_waits (
    id, session_id, project_id, kind, condition, pr_number, pr_url, note,
    baseline, created_at, fire_at, expires_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetSessionWait :one
SELECT * FROM session_waits WHERE id = ?;

-- name: ListPendingSessionWaits :many
SELECT * FROM session_waits
WHERE state = 'pending'
ORDER BY created_at, id;

-- name: ListPendingSessionWaitsBySession :many
SELECT * FROM session_waits
WHERE session_id = ? AND state = 'pending'
ORDER BY created_at, id;

-- A wait resolves at most once: only a still-pending row moves, so a cancel
-- racing a wake (or a second evaluator pass) cannot resolve it twice.
-- name: ResolveSessionWait :execrows
UPDATE session_waits
SET state = sqlc.arg(state), outcome = sqlc.arg(outcome), message = sqlc.arg(message),
    resolved_at = sqlc.arg(resolved_at)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: UpdateSessionWaitTarget :execrows
UPDATE session_waits
SET pr_url = sqlc.arg(pr_url), baseline = sqlc.arg(baseline)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: RecordSessionWaitDeliveryFailure :execrows
UPDATE session_waits
SET delivery_attempts = delivery_attempts + 1, last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: DeleteResolvedSessionWaitsBefore :execrows
DELETE FROM session_waits
WHERE state <> 'pending' AND resolved_at < ?;
