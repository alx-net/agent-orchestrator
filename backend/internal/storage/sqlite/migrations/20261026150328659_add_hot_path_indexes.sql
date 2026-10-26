-- +goose Up
-- Hot-path lookups that otherwise full-scan their tables.
--
-- ListCurrentHeadReviewRunsBySessions runs on every board/session list read and
-- probes review_run once per row for a newer same-head pass; without an index
-- that is a scan inside a scan. The same prefix serves the per-session review
-- run reads. The only existing session index is partial (running rows only).
CREATE INDEX IF NOT EXISTS idx_review_run_session_head
    ON review_run(session_id, pr_url, target_sha, harness, created_at, id);

-- ListPRFactsBySessions matches PR comments and reviews to AO review runs by
-- github_review_id. Most runs never post a GitHub review, so index only those
-- that did.
CREATE INDEX IF NOT EXISTS idx_review_run_github_review
    ON review_run(github_review_id)
    WHERE github_review_id != '';

-- SelectNextQueuedConversationTurn joins messages to turns by turn_id and polls
-- during handoff; deleting a turn also nulls these columns through ON DELETE
-- SET NULL.
CREATE INDEX IF NOT EXISTS idx_conversation_messages_turn
    ON conversation_messages(turn_id)
    WHERE turn_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversation_activities_turn
    ON conversation_activities(turn_id)
    WHERE turn_id IS NOT NULL;

-- DeleteSession clears change_log rows by session_id, and the sessions foreign
-- key check probes the same column.
CREATE INDEX IF NOT EXISTS idx_change_log_session
    ON change_log(session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_change_log_session;
DROP INDEX IF EXISTS idx_conversation_activities_turn;
DROP INDEX IF EXISTS idx_conversation_messages_turn;
DROP INDEX IF EXISTS idx_review_run_github_review;
DROP INDEX IF EXISTS idx_review_run_session_head;
