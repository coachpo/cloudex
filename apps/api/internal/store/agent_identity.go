package store

import (
	"context"
	"database/sql"
)

func (s *Store) AgentAuthority(ctx context.Context, candidate []byte) ([]byte, error) {
	return s.persistentSecret(ctx, "agent_ca", candidate)
}

func (s *Store) persistentSecret(ctx context.Context, name string, candidate []byte) (sealed []byte, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO secrets VALUES(?,?)", name, candidate); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT encrypted_value FROM secrets WHERE name=?", name).Scan(&sealed)
	})
	return
}
