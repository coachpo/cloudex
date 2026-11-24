package store

import (
	"context"
	"database/sql"
)

func (s *Store) AgentAuthority(ctx context.Context, candidate []byte) (sealed []byte, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO secrets VALUES('agent_ca',?)", candidate); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT encrypted_value FROM secrets WHERE name='agent_ca'").Scan(&sealed)
	})
	return
}
