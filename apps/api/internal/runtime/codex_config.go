package runtime

import (
	"context"
)

// CodexConfigSeed is the exact initial config.toml in the Workspace image.
const CodexConfigSeed = "cli_auth_credentials_store = \"file\"\n"

type CodexConfig struct {
	AgentsMD   string `json:"agents_md"`
	ConfigTOML string `json:"config_toml"`
	AuthJSON   string `json:"auth_json"`
}

type CodexConfigWriter interface {
	ApplyCodexConfig(context.Context, Spec, string, CodexConfig) error
}

func (d *Docker) ApplyCodexConfig(ctx context.Context, s Spec, id string, config CodexConfig) error {
	if err := d.checkWorkspaceBeforeStart(ctx, s, id); err != nil {
		return err
	}
	files := []workspaceFile{
		{"AGENTS.md", config.AgentsMD, 0644},
		{"config.toml", config.ConfigTOML, 0600},
	}
	if config.AuthJSON != "" {
		files = append(files, workspaceFile{"auth.json", config.AuthJSON, 0600})
	}
	return d.copyWorkspaceFiles(ctx, id, "/home/coder/.codex", files, 10001)
}
