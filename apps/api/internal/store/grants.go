package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	GrantActive         = "active"
	GrantMissingScope   = "missing_scope"
	GrantReauthRequired = "reauth_required"
	GrantDisconnected   = "disconnected"
)

var ErrGrantReferenced = fmt.Errorf("%w: grant still has Workspace bindings", ErrConflict)
var ErrGrantUnavailable = errors.New("grant is unavailable")

type Grant struct {
	ID                  string    `json:"id"`
	RegistrationID      string    `json:"registration_id"`
	Channel             string    `json:"channel"`
	Issuer              string    `json:"issuer"`
	ClientID            string    `json:"client_id"`
	Subject             string    `json:"subject"`
	UpstreamUserID      string    `json:"upstream_user_id,omitempty"`
	UpstreamWorkspaceID string    `json:"upstream_workspace_id,omitempty"`
	DisplayName         string    `json:"display_name,omitempty"`
	Email               string    `json:"email,omitempty"`
	Status              string    `json:"status"`
	Reason              string    `json:"reason,omitempty"`
	Scope               string    `json:"scope"`
	Resource            string    `json:"resource,omitempty"`
	Credential          []byte    `json:"-"`
	ExpiresAt           time.Time `json:"expires_at"`
	EarliestRefreshAt   time.Time `json:"-"`
	RefreshRetryAt      time.Time `json:"-"`
	TokenVersion        int64     `json:"-"`
	GrantEpoch          int64     `json:"grant_epoch"`
	RefreshAttemptID    string    `json:"-"`
}

type GrantWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func GrantRegistrationID(channel, issuer, clientID string) string {
	digest := sha256.New()
	for _, field := range []string{channel, issuer, clientID} {
		fmt.Fprintf(digest, "%d:%s", len(field), field)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

const grantFields = `g.id,g.registration_id,g.channel,r.issuer,r.client_id,g.subject,g.upstream_user_id,
 g.upstream_workspace_id,g.display_name,g.email,g.status,g.reason,g.scope,g.resource,g.expires_at,
 g.earliest_refresh_at,g.retry_at,g.token_version,g.grant_epoch,g.refresh_attempt_id`

func scanGrant(row scanner, secret bool) (grant Grant, err error) {
	var expires, earliest, retry int64
	fields := []any{&grant.ID, &grant.RegistrationID, &grant.Channel, &grant.Issuer, &grant.ClientID,
		&grant.Subject, &grant.UpstreamUserID, &grant.UpstreamWorkspaceID, &grant.DisplayName, &grant.Email,
		&grant.Status, &grant.Reason, &grant.Scope, &grant.Resource, &expires, &earliest, &retry, &grant.TokenVersion,
		&grant.GrantEpoch, &grant.RefreshAttemptID}
	if secret {
		fields = append(fields, &grant.Credential)
	}
	err = missing(row.Scan(fields...))
	grant.ExpiresAt, grant.EarliestRefreshAt, grant.RefreshRetryAt = readGrantTime(expires), readGrantTime(earliest), readGrantTime(retry)
	return
}

func readGrantTime(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}

func writeGrantTime(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

func (s *Store) Grant(ctx context.Context, id string) (Grant, error) {
	return scanGrant(s.db.QueryRowContext(ctx, "SELECT "+grantFields+
		",g.credential FROM managed_grants g JOIN oauth_registrations r ON r.id=g.registration_id WHERE g.id=?", id), true)
}

func (s *Store) Grants(ctx context.Context) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+grantFields+
		" FROM managed_grants g JOIN oauth_registrations r ON r.id=g.registration_id ORDER BY g.display_name COLLATE NOCASE,g.id")
	return collect(rows, err, func(row scanner) (Grant, error) { return scanGrant(row, false) })
}

func (s *Store) AddGrant(ctx context.Context, grant Grant) error {
	if grant.RegistrationID != GrantRegistrationID(grant.Channel, grant.Issuer, grant.ClientID) ||
		(grant.Status != GrantActive && grant.Status != GrantMissingScope) {
		return ErrConflict
	}
	registeredSubject := ""
	if grant.Channel == "chatgpt" {
		registeredSubject = grant.Subject
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO oauth_registrations (id,channel,issuer,client_id,subject) VALUES (?,?,?,?,?)
 ON CONFLICT(channel,issuer,client_id) DO NOTHING`, grant.RegistrationID, grant.Channel, grant.Issuer, grant.ClientID, registeredSubject)
		if err != nil {
			return err
		}
		var subject string
		if err := tx.QueryRowContext(ctx, "SELECT subject FROM oauth_registrations WHERE id=?", grant.RegistrationID).Scan(&subject); err != nil {
			return err
		}
		if subject != registeredSubject {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO managed_grants
 (id,registration_id,channel,subject,upstream_user_id,upstream_workspace_id,display_name,email,status,reason,scope,resource,credential,expires_at,earliest_refresh_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, grant.ID, grant.RegistrationID, grant.Channel, grant.Subject, grant.UpstreamUserID,
			grant.UpstreamWorkspaceID, grant.DisplayName, grant.Email, grant.Status, grant.Reason, grant.Scope, grant.Resource,
			grant.Credential, writeGrantTime(grant.ExpiresAt), writeGrantTime(grant.EarliestRefreshAt))
		return affected(result, err, ErrConflict)
	})
}

func (s *Store) ReplaceGrant(ctx context.Context, grant Grant, expectedEpoch int64) error {
	if grant.RegistrationID != GrantRegistrationID(grant.Channel, grant.Issuer, grant.ClientID) ||
		(grant.Status != GrantActive && grant.Status != GrantMissingScope) {
		return ErrConflict
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE managed_grants SET display_name=?,email=?,status=?,reason=?,scope=?,resource=?,credential=?,
 expires_at=?,earliest_refresh_at=?,retry_at=0,token_version=token_version+1,grant_epoch=grant_epoch+1,refresh_attempt_id=''
 WHERE id=? AND grant_epoch=? AND registration_id=? AND channel=? AND subject=? AND upstream_user_id=? AND upstream_workspace_id=?`,
			grant.DisplayName, grant.Email, grant.Status, grant.Reason, grant.Scope, grant.Resource, grant.Credential,
			writeGrantTime(grant.ExpiresAt), writeGrantTime(grant.EarliestRefreshAt), grant.ID, expectedEpoch, grant.RegistrationID,
			grant.Channel, grant.Subject, grant.UpstreamUserID, grant.UpstreamWorkspaceID)
		if err := affected(result, err, ErrConflict); err != nil {
			return err
		}
		return revokeGrantCredentials(ctx, tx, grant.ID)
	})
}

// BeginGrantRefresh records the attempt before an exchange can consume the
// refresh token. A process restart cannot safely send that token again.
func (s *Store) BeginGrantRefresh(ctx context.Context, id string, epoch, version int64, attempt string) error {
	if attempt == "" {
		return ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE managed_grants SET refresh_attempt_id=?
 WHERE id=? AND grant_epoch=? AND token_version=? AND status='active' AND refresh_attempt_id=''`, attempt, id, epoch, version)
	return affected(result, err, ErrConflict)
}

func (s *Store) CompleteGrantRefresh(ctx context.Context, grant Grant, attempt string) error {
	if attempt == "" || grant.Status != GrantActive {
		return ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE managed_grants SET credential=?,expires_at=?,earliest_refresh_at=?,retry_at=?,
 token_version=token_version+1,refresh_attempt_id='',reason=''
 WHERE id=? AND grant_epoch=? AND token_version=? AND refresh_attempt_id=? AND status='active' AND registration_id=?
 AND channel=? AND subject=? AND upstream_user_id=? AND upstream_workspace_id=? AND scope=? AND resource=?`, grant.Credential,
		writeGrantTime(grant.ExpiresAt), writeGrantTime(grant.EarliestRefreshAt), writeGrantTime(grant.RefreshRetryAt), grant.ID, grant.GrantEpoch, grant.TokenVersion,
		attempt, grant.RegistrationID, grant.Channel, grant.Subject, grant.UpstreamUserID, grant.UpstreamWorkspaceID, grant.Scope, grant.Resource)
	return affected(result, err, ErrConflict)
}

func (s *Store) FailGrantRefresh(ctx context.Context, id string, epoch, version int64, attempt, status, reason string, retry ...time.Time) error {
	if attempt == "" || (status != GrantActive && status != GrantReauthRequired && status != GrantMissingScope) {
		return ErrConflict
	}
	var retryAt int64
	if len(retry) > 0 {
		retryAt = writeGrantTime(retry[0])
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE managed_grants SET status=?,reason=?,retry_at=?,refresh_attempt_id='',
 grant_epoch=grant_epoch+CASE WHEN ?='active' THEN 0 ELSE 1 END
 WHERE id=? AND grant_epoch=? AND token_version=? AND refresh_attempt_id=? AND status='active'`,
			status, reason, retryAt, status, id, epoch, version, attempt)
		if err := affected(result, err, ErrConflict); err != nil {
			return err
		}
		if status != GrantActive {
			return revokeGrantCredentials(ctx, tx, id)
		}
		return nil
	})
}

func (s *Store) RecoverGrantRefreshes(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := invalidateGatewayContext(ctx, tx, "grant_id IN (SELECT id FROM managed_grants WHERE refresh_attempt_id<>'')"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE managed_grants SET grant_epoch=grant_epoch+1,refresh_attempt_id='',status='reauth_required',
 reason='refresh_outcome_unknown',retry_at=0 WHERE refresh_attempt_id<>''`)
		return err
	})
}

func (s *Store) DisconnectGrant(ctx context.Context, id string, expectedEpoch int64) (epoch int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, "SELECT status,grant_epoch FROM managed_grants WHERE id=?", id).Scan(&status, &epoch)
		if err != nil {
			return missing(err)
		}
		if epoch != expectedEpoch {
			return ErrConflict
		}
		if status == GrantDisconnected {
			return nil
		}
		epoch++
		_, err = tx.ExecContext(ctx, `UPDATE managed_grants SET status='disconnected',reason='remote_revocation_unconfirmed',
 credential=NULL,expires_at=0,earliest_refresh_at=0,retry_at=0,refresh_attempt_id='',grant_epoch=? WHERE id=?`, epoch, id)
		if err != nil {
			return err
		}
		return revokeGrantCredentials(ctx, tx, id)
	})
	return
}

func (s *Store) FinishGrantRevocation(ctx context.Context, id string, epoch int64, reason string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE managed_grants SET reason=? WHERE id=? AND grant_epoch=? AND status='disconnected'", reason, id, epoch)
	return affected(result, err, ErrConflict)
}

func (s *Store) DeleteGrant(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var referenced bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM workspace_grant_bindings WHERE grant_id=?)", id).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return ErrGrantReferenced
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM managed_grants WHERE id=?", id)
		return affected(result, err, ErrMissing)
	})
}

func (s *Store) GrantWorkspaces(ctx context.Context, id string) ([]GrantWorkspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.name FROM workspace_grant_bindings b JOIN workspaces w ON w.id=b.workspace_id
 WHERE b.grant_id=? AND w.deleted_at IS NULL ORDER BY w.name,w.id`, id)
	return collect(rows, err, func(row scanner) (w GrantWorkspace, err error) {
		err = row.Scan(&w.ID, &w.Name)
		return
	})
}
