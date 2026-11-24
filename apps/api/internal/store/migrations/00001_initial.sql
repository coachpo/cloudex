-- +goose Up
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE users (username TEXT PRIMARY KEY, password_hash TEXT NOT NULL);

CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  username TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL
);

CREATE TABLE secrets (name TEXT PRIMARY KEY, encrypted_value BLOB NOT NULL);

CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL COLLATE NOCASE,
  image TEXT NOT NULL,
  repo_url TEXT NOT NULL,
  branch TEXT NOT NULL,
  cpu_milli INTEGER NOT NULL CHECK (cpu_milli > 0),
  memory_mb INTEGER NOT NULL CHECK (memory_mb > 0),
  desired_state TEXT NOT NULL CHECK (desired_state IN ('running', 'stopped')),
  observed_state TEXT NOT NULL CHECK (observed_state IN ('creating','running','stopped','rebuilding','deleting','error','deleted')),
  generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  project_volume_revision INTEGER NOT NULL DEFAULT 0 CHECK (project_volume_revision >= 0),
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  deleted_at TEXT,
  paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
  start_unverified INTEGER NOT NULL DEFAULT 0 CHECK (start_unverified IN (0, 1)),
  observation_version INTEGER NOT NULL DEFAULT 0,
  assigned_host_id TEXT REFERENCES host_nodes(id),
  disk_bytes INTEGER NOT NULL DEFAULT 0 CHECK (disk_bytes >= 0)
);
CREATE UNIQUE INDEX workspace_live_name ON workspaces(name) WHERE deleted_at IS NULL;

CREATE TABLE workspace_repository_credentials (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
  host TEXT NOT NULL,
  username TEXT NOT NULL,
  token TEXT NOT NULL
);

CREATE TABLE containers (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  generation INTEGER NOT NULL,
  docker_id TEXT NOT NULL UNIQUE,
  image_id TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  host_id TEXT REFERENCES host_nodes(id),
  PRIMARY KEY (workspace_id, generation)
);

CREATE TABLE operations (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  type TEXT NOT NULL CHECK (type IN ('create','start','stop','rebuild','delete','reset')),
  status TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled')),
  target_generation INTEGER NOT NULL,
  purge INTEGER NOT NULL DEFAULT 0,
  phase INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  idempotency_key TEXT,
  request_hash TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  host_id TEXT REFERENCES host_nodes(id),
  retry_after INTEGER NOT NULL DEFAULT 0,
  reacquired_reservation INTEGER NOT NULL DEFAULT 0 CHECK (reacquired_reservation IN (0,1))
);
CREATE UNIQUE INDEX one_operation ON operations(workspace_id) WHERE status IN ('queued','running');
CREATE UNIQUE INDEX operation_key ON operations(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX operation_queue ON operations(status,created_at);

CREATE TABLE ssh_public_keys (public_key TEXT PRIMARY KEY);

CREATE TABLE host_nodes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  connection_type TEXT NOT NULL DEFAULT 'agent' CHECK (connection_type IN ('agent','ssh')),
  certificate_serial TEXT UNIQUE,
  ssh_address TEXT NOT NULL DEFAULT '',
  ssh_user TEXT NOT NULL DEFAULT '',
  ssh_host_key TEXT NOT NULL DEFAULT '',
  ssh_revision INTEGER NOT NULL DEFAULT 0,
  ssh_config BLOB,
  connection_error TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  engine TEXT NOT NULL DEFAULT '',
  os TEXT NOT NULL DEFAULT '',
  arch TEXT NOT NULL DEFAULT '',
  healthy INTEGER NOT NULL DEFAULT 0 CHECK (healthy IN (0,1)),
  max_operations INTEGER NOT NULL DEFAULT 1 CHECK (max_operations > 0),
  running_containers INTEGER NOT NULL DEFAULT 0 CHECK (running_containers >= 0),
  total_containers INTEGER NOT NULL DEFAULT 0 CHECK (total_containers >= running_containers),
  last_seen TEXT,
  last_heartbeat_unix INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  scheduling_state TEXT NOT NULL DEFAULT 'enabled' CHECK (scheduling_state IN ('enabled','cordoned','disabled')),
  scheduling_version INTEGER NOT NULL DEFAULT 0,
  max_operations_target INTEGER NOT NULL DEFAULT 3 CHECK (max_operations_target >= 1)
);

CREATE TABLE host_enrollment_tokens (
  token_hash TEXT PRIMARY KEY,
  host_id TEXT NOT NULL REFERENCES host_nodes(id),
  expires_unix INTEGER NOT NULL,
  used_at TEXT,
  revoked_at TEXT
);
CREATE INDEX host_enrollment_by_host ON host_enrollment_tokens(host_id);

CREATE TABLE host_capacity (
  host_id TEXT PRIMARY KEY REFERENCES host_nodes(id),
  cpu_allocatable INTEGER NOT NULL DEFAULT 0 CHECK (cpu_allocatable >= 0),
  memory_allocatable INTEGER NOT NULL DEFAULT 0 CHECK (memory_allocatable >= 0),
  disk_allocatable INTEGER NOT NULL DEFAULT 0 CHECK (disk_allocatable >= 0),
  cpu_reserved INTEGER NOT NULL DEFAULT 0 CHECK (cpu_reserved >= 0),
  memory_reserved INTEGER NOT NULL DEFAULT 0 CHECK (memory_reserved >= 0),
  disk_reserved INTEGER NOT NULL DEFAULT 0 CHECK (disk_reserved >= 0)
);

CREATE INDEX workspaces_by_host ON workspaces(assigned_host_id,created_at DESC) WHERE deleted_at IS NULL;

CREATE INDEX operation_host_status ON operations(host_id,status,created_at);

CREATE TABLE workspace_reservations (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
  host_id TEXT NOT NULL REFERENCES host_nodes(id),
  cpu_milli INTEGER NOT NULL CHECK (cpu_milli >= 0),
  memory_bytes INTEGER NOT NULL CHECK (memory_bytes >= 0),
  disk_bytes INTEGER NOT NULL CHECK (disk_bytes >= 0),
  status TEXT NOT NULL CHECK (status IN ('reserved','committed','retained','released')),
  updated_at TEXT NOT NULL
);
CREATE INDEX workspace_reservations_by_host ON workspace_reservations(host_id,status);

CREATE TABLE host_images (
  host_id TEXT NOT NULL REFERENCES host_nodes(id) ON DELETE CASCADE,
  image TEXT NOT NULL,
  ready INTEGER NOT NULL CHECK (ready IN (0,1)),
  PRIMARY KEY (host_id, image)
);

INSERT INTO host_nodes(id,name,created_at) VALUES('local','Local Host',strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT INTO host_capacity(host_id) VALUES('local');
