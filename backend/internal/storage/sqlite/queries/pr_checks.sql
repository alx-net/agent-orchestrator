-- name: UpsertPRCheck :exec
INSERT INTO pr_checks (pr_url, name, commit_hash, status, url, log_tail, created_at, conclusion, details,
    is_required, started_at, completed_at, workflow, description)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (pr_url, name, commit_hash) DO UPDATE SET
    status = excluded.status,
    url = excluded.url,
    log_tail = excluded.log_tail,
    conclusion = excluded.conclusion,
    details = excluded.details,
    is_required = excluded.is_required,
    started_at = excluded.started_at,
    completed_at = excluded.completed_at,
    workflow = excluded.workflow,
    description = excluded.description;

-- name: ListChecksByPR :many
SELECT pr_url, name, commit_hash, status, url, log_tail, created_at, conclusion, details,
    is_required, started_at, completed_at, workflow, description
FROM pr_checks WHERE pr_url = ? ORDER BY name, created_at;
