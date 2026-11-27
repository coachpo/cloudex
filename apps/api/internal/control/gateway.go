package control

import (
	"context"
	"errors"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type GatewayLifecycle interface {
	ReconcileGateway(context.Context, store.Workspace, runtime.Container, runtime.GatewayCredentialRuntime) error
	SuspendGateway(context.Context, string) error
}

func WorkspaceSpec(w store.Workspace) runtime.Spec { return spec(w, w.Generation) }

// ReconcileGateway verifies the recorded running instance on the Workspace's
// assigned Host. Provisioning failures leave container lifecycle success intact.
func (c *Control) ReconcileGateway(ctx context.Context, w store.Workspace) error {
	if c.Gateway == nil {
		return nil
	}
	check, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	current, record, err := c.Store.ReadyContainer(check, w.ID)
	if err != nil {
		if errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrMissing) {
			return c.Gateway.SuspendGateway(ctx, w.ID)
		}
		return errors.Join(err, c.Gateway.SuspendGateway(ctx, w.ID))
	}
	engine := c.EngineFor(current.HostID)
	instance, err := engine.Inspect(check, WorkspaceSpec(current))
	if err != nil || instance.ID != record.ID || instance.State != "running" || instance.StartedAt == "" {
		return c.Gateway.SuspendGateway(ctx, current.ID)
	}
	installer, ok := engine.(runtime.GatewayCredentialRuntime)
	if !ok {
		return errors.Join(errors.New("the Workspace runtime cannot verify gateway credentials"), c.Gateway.SuspendGateway(ctx, current.ID))
	}
	return c.Gateway.ReconcileGateway(check, current, instance, installer)
}
