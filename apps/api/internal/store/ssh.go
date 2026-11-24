package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Serialize final admission with authorization writes and lifecycle enqueue.
// The caller bounds accept; WorkspaceSSH releases its transaction first.
func (s *Store) AdmitSSH(ctx context.Context, id, key string, generation int, accept func() error) error {
	unlock, err := s.admission.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	w, a, busy, err := s.WorkspaceSSH(ctx, id)
	if err != nil {
		return err
	}
	if busy || w.Observed != "running" || w.Generation != generation || !a.Enabled || !slices.Contains(a.Keys, key) {
		return ErrBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return accept()
}

type SSHAccess struct {
	Enabled bool
	Keys    []string
}

func readSSH(ctx context.Context, tx *sql.Tx, id string) (a SSHAccess, err error) {
	a.Keys = []string{}
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM workspace_access WHERE workspace_id=?`, id).Scan(&a.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT public_key FROM ssh_public_keys WHERE workspace_id=? ORDER BY public_key`, id)
	a.Keys, err = collect(rows, err, func(r scanner) (string, error) { var key string; e := r.Scan(&key); return key, e })
	return
}

func (s *Store) WorkspaceSSH(ctx context.Context, id string) (w Workspace, a SSHAccess, busy bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var e error
		w, a, busy, e = workspaceSSH(ctx, tx, id)
		return e
	})
	return
}

func workspaceSSH(ctx context.Context, tx *sql.Tx, id string) (w Workspace, a SSHAccess, busy bool, err error) {
	w, err = findWorkspace(ctx, tx, id)
	if err != nil {
		return
	}
	a, err = readSSH(ctx, tx, id)
	if err != nil {
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND status IN ('queued','running'))`, id).Scan(&busy)
	return
}
func writeSSH(ctx context.Context, tx *sql.Tx, id string, keys []string) error {
	if len(keys) == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM workspace_access WHERE workspace_id=?`, id)
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_access VALUES(?,1)
  ON CONFLICT(workspace_id) DO UPDATE SET enabled=1`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ssh_public_keys WHERE workspace_id=?`, id); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ssh_public_keys VALUES(?,?)`, id, key); err != nil {
			return err
		}
	}
	return nil
}

// Save desired authorization and enqueue replacement of the container's
// authorized_keys snapshot in the same transaction.
func (s *Store) SetSSH(ctx context.Context, id string, keys []string, key, hash string) (o Operation, err error) {
	unlock, err := s.admission.lock(ctx, id)
	if err != nil {
		return o, err
	}
	defer unlock()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var e error
		o, e = existingOperation(ctx, tx, key, hash)
		if !errors.Is(e, ErrMissing) {
			return e
		}
		w, e := findWorkspace(ctx, tx, id)
		if e != nil {
			return e
		}
		if e = checkIdle(ctx, tx, id); e != nil {
			return e
		}
		if e = writeSSH(ctx, tx, id, keys); e != nil {
			return e
		}
		o, e = insertOp(ctx, tx, w, "rebuild", w.Generation+1, false, key, hash)
		if e != nil {
			return e
		}
		if e = reacquireReservation(ctx, tx, w, o); e != nil {
			return e
		}
		o.ApplySSH = true
		_, e = tx.ExecContext(ctx, `UPDATE operations SET apply_ssh=1 WHERE id=?`, o.ID)
		return e
	})
	return
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
