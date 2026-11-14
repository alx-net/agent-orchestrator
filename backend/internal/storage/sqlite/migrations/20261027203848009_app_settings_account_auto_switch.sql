-- +goose Up
-- On by default: a chat whose turn hits its account's usage limit moves to
-- another signed-in account with usage left and runs the turn again there.
ALTER TABLE app_settings ADD COLUMN account_auto_switch INTEGER NOT NULL DEFAULT 1
    CHECK (account_auto_switch IN (0, 1));

-- +goose Down
ALTER TABLE app_settings DROP COLUMN account_auto_switch;
