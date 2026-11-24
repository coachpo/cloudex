-- +goose Up
CREATE TABLE workspace_repository_credentials (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
  host TEXT NOT NULL,
  username TEXT NOT NULL,
  token TEXT NOT NULL
);

-- +goose Down
DROP TABLE workspace_repository_credentials;
