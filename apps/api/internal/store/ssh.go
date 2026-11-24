package store

import (
	"context"
	"database/sql"
)

// Serialize final admission with key replacement and lifecycle changes.
func (s *Store) AdmitSSH(ctx context.Context, id, key string, generation int, accept func() error) error {
	unlock, err := s.LockWorkspace(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	s.sshAdmission.RLock()
	defer s.sshAdmission.RUnlock()
	w, _, err := s.ReadyContainer(ctx, id)
	if err != nil {
		return err
	}
	allowed, err := s.HasSSHKey(ctx, key)
	if err != nil {
		return err
	}
	if !allowed || w.Generation != generation {
		return ErrBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return accept()
}

func (s *Store) SSHKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT public_key FROM ssh_public_keys ORDER BY public_key`)
	return collect(rows, err, func(r scanner) (string, error) {
		var key string
		err := r.Scan(&key)
		return key, err
	})
}

func (s *Store) HasSSHKey(ctx context.Context, key string) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ssh_public_keys WHERE public_key=?)`, key).Scan(&allowed)
	return allowed, err
}

func (s *Store) SetSSHKeys(ctx context.Context, keys []string) error {
	s.sshAdmission.Lock()
	defer s.sshAdmission.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ssh_public_keys`); err != nil {
			return err
		}
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ssh_public_keys VALUES(?)`, key); err != nil {
				return err
			}
		}
		return nil
	})
}

// SSHSyncGenerations includes the candidate of a lifecycle operation that
// may have started Docker before publishing the new generation.
func (s *Store) SSHSyncGenerations(ctx context.Context, id string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT generation FROM workspaces WHERE id=?1 AND deleted_at IS NULL AND generation>0
 UNION SELECT target_generation FROM operations WHERE workspace_id=?1 AND status IN ('queued','running') AND target_generation>0`, id)
	return collect(rows, err, func(r scanner) (int, error) {
		var generation int
		err := r.Scan(&generation)
		return generation, err
	})
}

func (s *Store) GatewayIdentity(ctx context.Context, candidate []byte) (sealed []byte, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO secrets VALUES('ssh_gateway',?)`, candidate); e != nil {
			return e
		}
		return tx.QueryRowContext(ctx, `SELECT encrypted_value FROM secrets WHERE name='ssh_gateway'`).Scan(&sealed)
	})
	return
}
