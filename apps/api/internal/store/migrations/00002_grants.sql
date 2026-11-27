-- +goose Up
CREATE TABLE oauth_registrations (
  id TEXT PRIMARY KEY,
  channel TEXT NOT NULL CHECK (channel IN ('codex', 'chatgpt')),
  issuer TEXT NOT NULL CHECK (issuer <> ''),
  client_id TEXT NOT NULL CHECK (client_id <> ''),
  subject TEXT NOT NULL,
  CHECK ((channel = 'codex' AND subject = '') OR (channel = 'chatgpt' AND subject <> '')),
  UNIQUE (channel, issuer, client_id),
  UNIQUE (id, channel)
);

CREATE TABLE managed_grants (
  id TEXT PRIMARY KEY,
  registration_id TEXT NOT NULL,
  channel TEXT NOT NULL,
  subject TEXT NOT NULL CHECK (subject <> ''),
  upstream_user_id TEXT NOT NULL,
  upstream_workspace_id TEXT NOT NULL,
  display_name TEXT NOT NULL,
  email TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('active', 'missing_scope', 'reauth_required', 'disconnected')),
  reason TEXT NOT NULL,
  scope TEXT NOT NULL,
  resource TEXT NOT NULL,
  credential BLOB,
  expires_at INTEGER NOT NULL,
  earliest_refresh_at INTEGER NOT NULL,
  retry_at INTEGER NOT NULL DEFAULT 0,
  token_version INTEGER NOT NULL DEFAULT 1 CHECK (token_version > 0),
  grant_epoch INTEGER NOT NULL DEFAULT 1 CHECK (grant_epoch > 0),
  refresh_attempt_id TEXT NOT NULL DEFAULT '',
  FOREIGN KEY (registration_id, channel) REFERENCES oauth_registrations(id, channel),
  CHECK (channel <> 'codex' OR (upstream_user_id <> '' AND upstream_workspace_id <> '')),
  CHECK ((status = 'disconnected' AND credential IS NULL) OR (status <> 'disconnected' AND credential IS NOT NULL AND length(credential) > 0))
);
CREATE UNIQUE INDEX codex_workspace_grant ON managed_grants(registration_id, subject, upstream_workspace_id) WHERE channel = 'codex';
CREATE UNIQUE INDEX chatgpt_registration_grant ON managed_grants(registration_id) WHERE channel = 'chatgpt';

CREATE TABLE workspace_grant_bindings (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  grant_id TEXT REFERENCES managed_grants(id) ON DELETE RESTRICT,
  binding_revision INTEGER NOT NULL CHECK (binding_revision > 0)
);
CREATE INDEX grant_references ON workspace_grant_bindings(grant_id) WHERE grant_id IS NOT NULL;

CREATE TABLE gateway_credentials (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  host_id TEXT NOT NULL,
  docker_id TEXT NOT NULL,
  started_at TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK (generation > 0),
  grant_id TEXT NOT NULL,
  binding_revision INTEGER NOT NULL CHECK (binding_revision > 0),
  grant_epoch INTEGER NOT NULL CHECK (grant_epoch > 0),
  digest TEXT NOT NULL UNIQUE CHECK (length(digest) = 64),
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'revoked'))
);
CREATE INDEX credentials_by_grant ON gateway_credentials(grant_id) WHERE status <> 'revoked';

CREATE TABLE gateway_submissions (
  credential_digest TEXT NOT NULL REFERENCES gateway_credentials(digest) ON DELETE CASCADE,
  run_id TEXT NOT NULL CHECK (run_id <> ''),
  turn_id TEXT NOT NULL CHECK (turn_id <> ''),
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  status TEXT NOT NULL CHECK (status <> ''),
  model TEXT NOT NULL CHECK (model <> ''),
  response_id TEXT NOT NULL,
  call_ids TEXT NOT NULL CHECK (json_valid(call_ids)),
  PRIMARY KEY (credential_digest, run_id, turn_id)
);

CREATE TABLE gateway_submission_references (
  workspace_id TEXT NOT NULL REFERENCES gateway_credentials(workspace_id) ON DELETE CASCADE,
  instance_scope TEXT NOT NULL CHECK (length(instance_scope) = 64),
  session_digest TEXT NOT NULL CHECK (length(session_digest) = 64),
  reference_digest TEXT NOT NULL CHECK (length(reference_digest) = 64),
  PRIMARY KEY (workspace_id, instance_scope, session_digest, reference_digest)
);

-- +goose Down
DROP TABLE gateway_submission_references;
DROP TABLE gateway_submissions;
DROP TABLE gateway_credentials;
DROP TABLE workspace_grant_bindings;
DROP TABLE managed_grants;
DROP TABLE oauth_registrations;
