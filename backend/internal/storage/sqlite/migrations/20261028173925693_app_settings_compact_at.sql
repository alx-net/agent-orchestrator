-- +goose Up
-- The token count at which Claude Code agents AO launches compact their
-- conversation. 0 leaves it to the repository's and user's Claude Code settings.
ALTER TABLE app_settings ADD COLUMN compact_at INTEGER NOT NULL DEFAULT 350000
    CHECK (compact_at = 0 OR compact_at BETWEEN 100000 AND 900000);

-- +goose Down
ALTER TABLE app_settings DROP COLUMN compact_at;
