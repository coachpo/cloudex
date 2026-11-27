package store

import (
	"context"
	"github.com/google/uuid"
)

const DefaultWorkspaceImage = "cloudex/workspace:codex-0.172.0"

type Settings struct {
	SSHHost            string
	AgentPublicAddress string
	WorkspaceImage     string
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var settings Settings
	err := s.db.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM meta WHERE key='setting.ssh_host'), ''),
 COALESCE((SELECT value FROM meta WHERE key='setting.agent_public_address'), ''),
 COALESCE((SELECT value FROM meta WHERE key='setting.workspace_image'), ?)`, DefaultWorkspaceImage).
		Scan(&settings.SSHHost, &settings.AgentPublicAddress, &settings.WorkspaceImage)
	return settings, err
}

func (s *Store) SetAccessSettings(ctx context.Context, sshHost, agentAddress string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('setting.ssh_host',?),('setting.agent_public_address',?)
 ON CONFLICT (key) DO UPDATE SET value=excluded.value`, sshHost, agentAddress)
	return err
}

func (s *Store) SetWorkspaceImage(ctx context.Context, image string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('setting.workspace_image',?)
 ON CONFLICT (key) DO UPDATE SET value=excluded.value`, image)
	return err
}

func (s *Store) DesiredWorkspaceImages(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE((SELECT value FROM meta WHERE key='setting.workspace_image'), ?) AS image
 UNION SELECT image FROM workspaces WHERE deleted_at IS NULL ORDER BY image`, DefaultWorkspaceImage)
	return collect(rows, err, func(r scanner) (image string, err error) {
		err = r.Scan(&image)
		return image, err
	})
}

func (s *Store) AgentHostID(ctx context.Context) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta VALUES('gateway.agent_host_id',?)`, uuid.New().URN()); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='gateway.agent_host_id'`).Scan(&id)
	return id, err
}
