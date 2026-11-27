package control

import (
	"context"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type credentialEngine struct {
	*lifecycleEngine
	runtime.GatewayCredentialRuntime
}

type lifecycleGateway struct {
	reconciled []runtime.Container
	suspended  []string
}

func (g *lifecycleGateway) ReconcileGateway(_ context.Context, _ store.Workspace, instance runtime.Container, _ runtime.GatewayCredentialRuntime) error {
	g.reconciled = append(g.reconciled, instance)
	return nil
}

func (g *lifecycleGateway) SuspendGateway(_ context.Context, id string) error {
	g.suspended = append(g.suspended, id)
	return nil
}

func TestGatewayReconciliationRequiresCurrentRunningInstance(t *testing.T) {
	db, w, instance := seedRecoveryWorkspace(t, "running", nil)
	engine := &credentialEngine{lifecycleEngine: &lifecycleEngine{containers: map[int]*runtime.Container{w.Generation: &instance}}}
	gateway := &lifecycleGateway{}
	c := &Control{Store: db, Engine: engine, Gateway: gateway}
	check := func(reconciled, suspended int) {
		t.Helper()
		if err := c.ReconcileGateway(t.Context(), w); err != nil {
			t.Fatal(err)
		}
		if len(gateway.reconciled) != reconciled || len(gateway.suspended) != suspended {
			t.Fatalf("gateway: observed %d verified instances and %d suspensions", len(gateway.reconciled), len(gateway.suspended))
		}
	}
	check(1, 0)
	engine.inspectErr = runtime.ErrHostUnavailable
	check(1, 1)
	instance.StartedAt = "new-start-reported-by-Docker"
	check(2, 1)
	if got := gateway.reconciled[1]; got.ID != instance.ID || got.StartedAt != instance.StartedAt {
		t.Fatal("gateway reconciliation did not use the recovered Docker startup")
	}
	instance.ID = "different-running-container"
	check(2, 2)
	instance.ID = gateway.reconciled[0].ID
	if _, err := db.Enqueue(t.Context(), w.ID, "stop", false, "", ""); err != nil {
		t.Fatal(err)
	}
	check(2, 3)
}
