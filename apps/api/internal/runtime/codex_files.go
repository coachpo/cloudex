package runtime

import "context"

// CodexConfigSeed is the exact initial config.toml in the Workspace image.
const CodexConfigSeed = "cli_auth_credentials_store = \"file\"\n"

type InitialCodexFiles struct {
	AgentsMD   string `json:"agents_md"`
	ConfigTOML string `json:"config_toml"`
}

type InitialCodexFilesWriter interface {
	WriteInitialCodexFiles(context.Context, Spec, string, InitialCodexFiles) error
}

func (d *Docker) WriteInitialCodexFiles(ctx context.Context, s Spec, id string, config InitialCodexFiles) error {
	if err := d.checkWorkspaceBeforeStart(ctx, s, id); err != nil {
		return err
	}
	var files []workspaceFile
	if config.AgentsMD != "" {
		files = append(files, workspaceFile{"AGENTS.md", config.AgentsMD, 0644})
	}
	if config.ConfigTOML != "" {
		files = append(files, workspaceFile{"config.toml", config.ConfigTOML, 0600})
	}
	if len(files) == 0 {
		return nil
	}
	return d.copyWorkspaceFiles(ctx, id, "/home/coder/.codex", files, 10001)
}
