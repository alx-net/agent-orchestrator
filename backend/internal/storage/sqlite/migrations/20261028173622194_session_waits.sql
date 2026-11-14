-- +goose Up
-- One zero-token wait an agent registered with `ao wait`: what it waits for and
-- how AO resolved it. The daemon evaluates pending rows against the PR facts
-- the SCM observer already persists (or a timer) and wakes the session once.
-- baseline is the JSON snapshot taken at registration (head SHA, known review
-- and comment ids) so "new" means new since the agent asked. Resolved rows
-- keep the delivered message for diagnosis until the daemon prunes them.
CREATE TABLE session_waits (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (kind IN ('pr', 'time')),
    condition TEXT NOT NULL DEFAULT '' CHECK (condition IN ('', 'checks', 'merge', 'review', 'any')),
    pr_number INTEGER NOT NULL DEFAULT 0 CHECK (pr_number >= 0),
    pr_url TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '' CHECK (length(note) <= 1000),
    baseline TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    fire_at TIMESTAMP,
    expires_at TIMESTAMP NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'fired', 'expired', 'cancelled')),
    outcome TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    resolved_at TIMESTAMP,
    delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    CHECK (
        (kind = 'pr' AND condition <> '' AND pr_number > 0)
        OR (kind = 'time' AND condition = '' AND pr_number = 0 AND fire_at IS NOT NULL)
    ),
    CHECK ((state = 'pending' AND resolved_at IS NULL) OR (state <> 'pending' AND resolved_at IS NOT NULL))
);

CREATE INDEX idx_session_waits_pending
    ON session_waits(state, session_id, created_at, id);

-- +goose Down
DROP INDEX IF EXISTS idx_session_waits_pending;
DROP TABLE IF EXISTS session_waits;
