package store

import (
	"context"
	"database/sql"
)

type Binding struct {
	WorkspaceID     string `json:"workspace_id"`
	GrantID         string `json:"grant_id,omitempty"`
	BindingRevision int64  `json:"binding_revision"`
}

func (s *Store) Binding(ctx context.Context, workspaceID string) (Binding, error) {
	return readBinding(ctx, s.db, workspaceID)
}

func readBinding(ctx context.Context, db queryer, workspaceID string) (Binding, error) {
	b := Binding{WorkspaceID: workspaceID}
	err := db.QueryRowContext(ctx, `SELECT COALESCE(b.grant_id,''),COALESCE(b.binding_revision,0)
 FROM workspaces w LEFT JOIN workspace_grant_bindings b ON b.workspace_id=w.id
 WHERE w.id=? AND w.deleted_at IS NULL`, workspaceID).Scan(&b.GrantID, &b.BindingRevision)
	return b, missing(err)
}

func checkGrantEpoch(ctx context.Context, db queryer, grantID string, epoch int64) error {
	var status string
	var actualEpoch int64
	if err := db.QueryRowContext(ctx, "SELECT status,grant_epoch FROM managed_grants WHERE id=?", grantID).Scan(&status, &actualEpoch); err != nil {
		return missing(err)
	}
	if actualEpoch != epoch {
		return ErrConflict
	}
	if status != GrantActive {
		return ErrGrantUnavailable
	}
	return nil
}

func (s *Store) SetBinding(ctx context.Context, workspaceID, grantID string, expectedRevision, expectedGrantEpoch int64) (Binding, error) {
	var binding Binding
	err := s.tx(ctx, func(tx *sql.Tx) error {
		current, err := readBinding(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		binding = current
		if current.GrantID == grantID {
			return nil
		}
		if current.BindingRevision != expectedRevision {
			return ErrConflict
		}
		if grantID != "" {
			if err := checkGrantEpoch(ctx, tx, grantID, expectedGrantEpoch); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_grant_bindings (workspace_id,grant_id,binding_revision)
 VALUES (?,NULLIF(?,''),?) ON CONFLICT(workspace_id) DO UPDATE SET
 grant_id=excluded.grant_id,binding_revision=excluded.binding_revision`, workspaceID, grantID, current.BindingRevision+1); err != nil {
			return err
		}
		if err := invalidateGatewayContext(ctx, tx, "workspace_id=?", workspaceID); err != nil {
			return err
		}
		binding = Binding{WorkspaceID: workspaceID, GrantID: grantID, BindingRevision: current.BindingRevision + 1}
		return nil
	})
	return binding, err
}

func deleteWorkspaceGrant(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_credentials WHERE workspace_id=?", workspaceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM workspace_grant_bindings WHERE workspace_id=?", workspaceID)
	return err
}
