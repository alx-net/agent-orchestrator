-- +goose Up
-- Per-check provider facts for the session merge box: whether branch
-- protection requires the check, its run times, the workflow that produced it,
-- and a commit status's short description. Existing rows read as not required
-- with unknown times until the next observation refreshes them.
ALTER TABLE pr_checks ADD COLUMN is_required INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pr_checks ADD COLUMN started_at TIMESTAMP;
ALTER TABLE pr_checks ADD COLUMN completed_at TIMESTAMP;
ALTER TABLE pr_checks ADD COLUMN workflow TEXT NOT NULL DEFAULT '';
ALTER TABLE pr_checks ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pr_checks DROP COLUMN description;
ALTER TABLE pr_checks DROP COLUMN workflow;
ALTER TABLE pr_checks DROP COLUMN completed_at;
ALTER TABLE pr_checks DROP COLUMN started_at;
ALTER TABLE pr_checks DROP COLUMN is_required;
