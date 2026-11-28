-- +goose Up
ALTER TABLE gateway_submissions ADD COLUMN compaction_window INTEGER;

-- +goose Down
ALTER TABLE gateway_submissions DROP COLUMN compaction_window;
