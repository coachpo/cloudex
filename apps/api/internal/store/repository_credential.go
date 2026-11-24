package store

import (
	"context"
	"database/sql"
)

type RepositoryCredential struct {
	Host     string
	Username string
	Token    string
}

type RepositoryCredentialStatus struct {
	Configured bool   `json:"configured"`
	Username   string `json:"username"`
}

func (s *Store) RepositoryCredentialStatus(ctx context.Context, id string) (status RepositoryCredentialStatus, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(c.username,'')
 FROM workspaces w LEFT JOIN workspace_repository_credentials c ON c.workspace_id=w.id
 WHERE w.id=? AND w.deleted_at IS NULL`, id).Scan(&status.Username)
	status.Configured = status.Username != ""
	return status, missing(err)
}

func (s *Store) RepositoryCredential(ctx context.Context, id string) (c RepositoryCredential, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT host,username,token FROM workspace_repository_credentials WHERE workspace_id=?`, id).
		Scan(&c.Host, &c.Username, &c.Token)
	return c, missing(err)
}

func writeRepositoryCredential(ctx context.Context, tx *sql.Tx, id string, credential *RepositoryCredential) error {
	if credential == nil {
		_, err := tx.ExecContext(ctx, `DELETE FROM workspace_repository_credentials WHERE workspace_id=?`, id)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO workspace_repository_credentials(workspace_id,host,username,token)
 VALUES(?,?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET host=excluded.host,username=excluded.username,token=excluded.token`,
		id, credential.Host, credential.Username, credential.Token)
	return err
}

func (s *Store) SetRepositoryCredential(ctx context.Context, id string, credential *RepositoryCredential) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := findWorkspace(ctx, tx, id); err != nil {
			return err
		}
		// Keep the saved credential stable while a new container is being prepared or retried.
		if err := checkIdle(ctx, tx, id); err != nil {
			return err
		}
		return writeRepositoryCredential(ctx, tx, id, credential)
	})
}
