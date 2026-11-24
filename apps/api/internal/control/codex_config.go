package control

import (
	"context"
	"errors"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
)

func (c *Control) applyCodexConfig(ctx context.Context, engine runtime.Engine, target runtime.Spec, id string) error {
	settings, err := c.Store.CodexConfig(ctx, target.WorkspaceID)
	if err != nil || !settings.Managed {
		return err
	}
	writer, ok := engine.(runtime.CodexConfigWriter)
	if !ok {
		return errors.New("the Workspace runtime cannot apply Codex configuration")
	}
	config := runtime.CodexConfig{
		AgentsMD:   settings.AgentsMD,
		ConfigTOML: settings.ConfigTOML,
		AuthJSON:   settings.AuthJSON,
	}
	if err := writer.ApplyCodexConfig(ctx, target, id, config); err != nil {
		return err
	}
	return c.Store.MarkCodexConfigApplied(ctx, target.WorkspaceID, settings.Revision)
}
