package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrCodexConfigChanged = errors.New("Codex configuration changed. Reload it before saving.")

type CodexConfigSettings struct {
	Managed    bool   `json:"managed"`
	AgentsMD   string `json:"agents_md"`
	ConfigTOML string `json:"config_toml"`
	AuthJSON   string `json:"auth_json"`
}

type CodexConfig struct {
	CodexConfigSettings
	Revision        int64 `json:"revision"`
	AppliedRevision int64 `json:"applied_revision"`
}

func readCodexConfig(ctx context.Context, db queryer, id string) (c CodexConfig, err error) {
	err = db.QueryRowContext(ctx, `SELECT COALESCE(c.managed,0),COALESCE(c.revision,0),COALESCE(c.applied_revision,0),
 COALESCE(c.agents_md,''),COALESCE(c.config_toml,''),COALESCE(c.auth_json,'')
 FROM workspaces w LEFT JOIN workspace_codex_config c ON c.workspace_id=w.id
 WHERE w.id=? AND w.deleted_at IS NULL`, id).Scan(&c.Managed, &c.Revision, &c.AppliedRevision,
		&c.AgentsMD, &c.ConfigTOML, &c.AuthJSON)
	return c, missing(err)
}

func (s *Store) CodexConfig(ctx context.Context, id string) (CodexConfig, error) {
	return readCodexConfig(ctx, s.db, id)
}

func (s *Store) SaveCodexConfig(ctx context.Context, id string, revision int64, settings CodexConfigSettings) (c CodexConfig, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		current, err := readCodexConfig(ctx, tx, id)
		if err != nil {
			return err
		}
		if revision != current.Revision {
			return ErrCodexConfigChanged
		}
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND status IN ('queued','running'))`, id).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrBusy
		}
		if current.Revision != 0 && settings == current.CodexConfigSettings {
			c = current
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_codex_config(workspace_id,managed,revision,agents_md,config_toml,auth_json)
 VALUES(?,?,?,?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET managed=excluded.managed,revision=excluded.revision,
 agents_md=excluded.agents_md,config_toml=excluded.config_toml,auth_json=excluded.auth_json`,
			id, settings.Managed, revision+1, settings.AgentsMD, settings.ConfigTOML, settings.AuthJSON)
		if err != nil {
			return err
		}
		c, err = readCodexConfig(ctx, tx, id)
		return err
	})
	return
}

func (s *Store) MarkCodexConfigApplied(ctx context.Context, id string, revision int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE workspace_codex_config SET applied_revision=?
 WHERE workspace_id=? AND revision=? AND managed=1`, revision, id, revision)
	return affected(result, err, ErrCodexConfigChanged)
}
