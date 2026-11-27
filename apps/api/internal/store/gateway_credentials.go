package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	CredentialPending = "pending"
	CredentialActive  = "active"
	CredentialRevoked = "revoked"
)

type GatewayCredential struct {
	WorkspaceID     string
	HostID          string
	DockerID        string
	StartedAt       string
	GrantID         string
	Digest          string
	Status          string
	Generation      int
	BindingRevision int64
	GrantEpoch      int64
	Sequence        int64
}

func GatewayCredentialDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// GatewayCredentialScope identifies the running instance's conversation
// context across rotations of its gateway Bearer.
func GatewayCredentialScope(cred GatewayCredential) string {
	return GatewayCredentialDigest(fmt.Sprintf("%q %q %q %d %q %q %d %d", cred.WorkspaceID, cred.HostID, cred.DockerID,
		cred.Generation, cred.StartedAt, cred.GrantID, cred.BindingRevision, cred.GrantEpoch))
}

func gatewayHistoryContainerScope(cred GatewayCredential) string {
	return GatewayCredentialDigest(fmt.Sprintf("%q %q %d", cred.HostID, cred.DockerID, cred.Generation))
}

func validGatewayDigest(value string) bool {
	bytes, err := hex.DecodeString(value)
	return err == nil && len(bytes) == sha256.Size && value == strings.ToLower(value)
}

const credentialFields = `k.workspace_id,k.host_id,k.docker_id,k.started_at,k.grant_id,k.digest,k.status,
 k.generation,k.binding_revision,k.grant_epoch,k.sequence`

func credentialScanValues(cred *GatewayCredential) []any {
	return []any{&cred.WorkspaceID, &cred.HostID, &cred.DockerID, &cred.StartedAt, &cred.GrantID, &cred.Digest, &cred.Status,
		&cred.Generation, &cred.BindingRevision, &cred.GrantEpoch, &cred.Sequence}
}

func workspaceCredential(ctx context.Context, db queryer, workspaceID string) (GatewayCredential, error) {
	var cred GatewayCredential
	err := db.QueryRowContext(ctx, "SELECT "+credentialFields+" FROM gateway_credentials k WHERE k.workspace_id=?", workspaceID).
		Scan(credentialScanValues(&cred)...)
	return cred, missing(err)
}

func (s *Store) WorkspaceCredential(ctx context.Context, workspaceID string) (GatewayCredential, error) {
	return workspaceCredential(ctx, s.db, workspaceID)
}

const readyGatewayWorkspace = `w.deleted_at IS NULL AND w.observed_state='running' AND w.paused=0 AND w.start_unverified=0
 AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id=w.id AND o.status IN ('queued','running'))`

const readyGatewayInstance = ` FROM workspaces w JOIN containers c ON c.workspace_id=w.id AND c.generation=w.generation AND c.host_id=w.assigned_host_id
 JOIN workspace_grant_bindings b ON b.workspace_id=w.id JOIN managed_grants g ON g.id=b.grant_id WHERE `

func credentialInstanceReady(ctx context.Context, db queryer, cred GatewayCredential) error {
	var ready bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1"+readyGatewayInstance+readyGatewayWorkspace+`
 AND w.id=? AND w.assigned_host_id=? AND w.generation=? AND c.docker_id=? AND b.grant_id=? AND b.binding_revision=?
 AND g.grant_epoch=? AND g.status='active')`, cred.WorkspaceID, cred.HostID, cred.Generation, cred.DockerID, cred.GrantID,
		cred.BindingRevision, cred.GrantEpoch).Scan(&ready)
	if err == nil && !ready {
		return ErrBusy
	}
	return err
}

func (s *Store) IssueCredential(ctx context.Context, seed GatewayCredential) (GatewayCredential, error) {
	if seed.StartedAt == "" || !validGatewayDigest(seed.Digest) {
		return GatewayCredential{}, ErrConflict
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := credentialInstanceReady(ctx, tx, seed); err != nil {
			return err
		}
		if err := clearCredentialSubmissions(ctx, tx, "workspace_id=?", seed.WorkspaceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_submission_references WHERE workspace_id=? AND instance_scope<>?",
			seed.WorkspaceID, GatewayCredentialScope(seed)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_history_references WHERE workspace_id=? AND container_scope<>?",
			seed.WorkspaceID, gatewayHistoryContainerScope(seed)); err != nil {
			return err
		}
		seed.Status = CredentialPending
		err := tx.QueryRowContext(ctx, `INSERT INTO gateway_credentials
 (workspace_id,host_id,docker_id,started_at,generation,grant_id,binding_revision,grant_epoch,digest,sequence,status)
 VALUES (?,?,?,?,?,?,?,?,?,1,'pending') ON CONFLICT (workspace_id) DO UPDATE SET
 host_id=excluded.host_id,docker_id=excluded.docker_id,started_at=excluded.started_at,generation=excluded.generation,
 grant_id=excluded.grant_id,binding_revision=excluded.binding_revision,grant_epoch=excluded.grant_epoch,digest=excluded.digest,
 sequence=gateway_credentials.sequence+1,status='pending' WHERE gateway_credentials.digest<>excluded.digest RETURNING sequence`,
			seed.WorkspaceID, seed.HostID, seed.DockerID, seed.StartedAt, seed.Generation, seed.GrantID,
			seed.BindingRevision, seed.GrantEpoch, seed.Digest).Scan(&seed.Sequence)
		if err == sql.ErrNoRows {
			return ErrConflict
		}
		return err
	})
	return seed, err
}

func (s *Store) ActivateCredential(ctx context.Context, cred GatewayCredential) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		current, err := workspaceCredential(ctx, tx, cred.WorkspaceID)
		if err != nil {
			return err
		}
		cred.Status = current.Status
		if current != cred || current.Status == CredentialRevoked {
			return ErrConflict
		}
		if err := credentialInstanceReady(ctx, tx, cred); err != nil {
			return err
		}
		if current.Status == CredentialActive {
			return nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE gateway_credentials SET status='active' WHERE workspace_id=?", cred.WorkspaceID)
		return err
	})
}

func (s *Store) AuthenticateCredential(ctx context.Context, digest string) (GatewayCredential, error) {
	return authenticateCredential(ctx, s.db, digest)
}

func authenticateCredential(ctx context.Context, db queryer, digest string) (GatewayCredential, error) {
	var cred GatewayCredential
	var grantStatus string
	var grantEpoch int64
	destinations := append(credentialScanValues(&cred), &grantStatus, &grantEpoch)
	err := db.QueryRowContext(ctx, "SELECT "+credentialFields+`,g.status,g.grant_epoch
 FROM gateway_credentials k JOIN workspaces w ON w.id=k.workspace_id AND w.generation=k.generation AND w.assigned_host_id=k.host_id
 JOIN containers c ON c.workspace_id=w.id AND c.generation=w.generation AND c.host_id=k.host_id AND c.docker_id=k.docker_id
 JOIN workspace_grant_bindings b ON b.workspace_id=w.id AND b.binding_revision=k.binding_revision AND b.grant_id=k.grant_id
 JOIN managed_grants g ON g.id=b.grant_id WHERE k.digest=? AND `+readyGatewayWorkspace, digest).Scan(destinations...)
	if err != nil {
		return GatewayCredential{}, missing(err)
	}
	if grantStatus != GrantActive {
		return cred, ErrGrantUnavailable
	}
	if credentialValid := cred.Status == CredentialActive && cred.GrantEpoch == grantEpoch; !credentialValid {
		return GatewayCredential{}, ErrMissing
	}
	return cred, nil
}

func (s *Store) RevokeWorkspaceCredential(ctx context.Context, workspaceID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return revokeCredentials(ctx, tx, "workspace_id=?", workspaceID)
	})
}

func revokeGrantCredentials(ctx context.Context, tx *sql.Tx, grantID string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_history_references WHERE grant_id=?", grantID); err != nil {
		return err
	}
	return invalidateGatewayContext(ctx, tx, "grant_id=?", grantID)
}

func invalidateGatewayContext(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_submission_references WHERE workspace_id IN"+
		" (SELECT workspace_id FROM gateway_credentials WHERE "+predicate+")", args...); err != nil {
		return err
	}
	return revokeCredentials(ctx, tx, predicate, args...)
}

// Only local, constant SQL predicates are passed here. Revoked metadata retains
// the sequence fence. References can survive a Bearer change for the same
// instance and binding, but old running processes lose admission immediately.
func revokeCredentials(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	if err := clearCredentialSubmissions(ctx, tx, predicate, args...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE gateway_credentials SET status='revoked',sequence=sequence+1 WHERE status<>'revoked' AND "+predicate, args...)
	return err
}

func clearCredentialSubmissions(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM gateway_submissions WHERE credential_digest IN"+
		" (SELECT digest FROM gateway_credentials WHERE "+predicate+")", args...)
	return err
}
