package store

import "context"

const DefaultWorkspaceImage = "cloudex/workspace:local"

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

type InitialCodexFiles struct {
	AgentsMD   string `json:"agents_md"`
	ConfigTOML string `json:"config_toml"`
}

func (s *Store) InitialCodexFiles(ctx context.Context) (InitialCodexFiles, error) {
	var files InitialCodexFiles
	err := s.db.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM meta WHERE key='setting.codex_agents_md'), ''),
 COALESCE((SELECT value FROM meta WHERE key='setting.codex_config_toml'), '')`).
		Scan(&files.AgentsMD, &files.ConfigTOML)
	return files, err
}

func (s *Store) SetInitialCodexFiles(ctx context.Context, files InitialCodexFiles) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('setting.codex_agents_md',?),('setting.codex_config_toml',?)
 ON CONFLICT (key) DO UPDATE SET value=excluded.value`, files.AgentsMD, files.ConfigTOML)
	return err
}
