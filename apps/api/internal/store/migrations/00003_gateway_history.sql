-- +goose Up
ALTER TABLE gateway_submission_references ADD COLUMN tool_call_fingerprint TEXT;

CREATE TABLE gateway_history_references (
  workspace_id TEXT NOT NULL REFERENCES gateway_credentials(workspace_id) ON DELETE CASCADE,
  container_scope TEXT NOT NULL CHECK (length(container_scope) = 64),
  grant_id TEXT NOT NULL REFERENCES managed_grants(id) ON DELETE CASCADE,
  grant_epoch INTEGER NOT NULL CHECK (grant_epoch > 0),
  session_digest TEXT NOT NULL CHECK (length(session_digest) = 64),
  reference_digest TEXT NOT NULL CHECK (length(reference_digest) = 64),
  PRIMARY KEY (workspace_id, container_scope, grant_id, grant_epoch, session_digest, reference_digest)
);
CREATE INDEX gateway_history_by_grant ON gateway_history_references(grant_id);

-- +goose Down
DROP TABLE gateway_history_references;
ALTER TABLE gateway_submission_references DROP COLUMN tool_call_fingerprint;
