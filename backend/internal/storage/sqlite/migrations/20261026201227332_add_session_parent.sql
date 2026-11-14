-- +goose Up
-- The session whose `ao spawn` created this one, so clients can show an
-- orchestrator and its workers as a hierarchy. Written once at spawn, only for a
-- parent that exists in the same project; NULL for every other session. No
-- foreign key: live sessions are never deleted (only unlaunched seed and
-- preparation rows are), and a dangling id simply reads as a top-level session.
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT;

CREATE INDEX IF NOT EXISTS idx_sessions_parent
    ON sessions(parent_session_id)
    WHERE parent_session_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_parent;
ALTER TABLE sessions DROP COLUMN parent_session_id;
