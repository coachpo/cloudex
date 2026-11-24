-- +goose Up
CREATE TABLE workspace_codex_config (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  managed INTEGER NOT NULL CHECK (managed IN (0, 1)),
  revision INTEGER NOT NULL CHECK (revision > 0),
  agents_md TEXT NOT NULL,
  config_toml TEXT NOT NULL,
  auth_json TEXT NOT NULL,
  applied_revision INTEGER NOT NULL DEFAULT 0
);
