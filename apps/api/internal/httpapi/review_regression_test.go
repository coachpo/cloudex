package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func seedWorkspace(t *testing.T, db *store.Store, state string, keys []string) store.Workspace {
	t.Helper()
	ctx := t.Context()
	w := store.Workspace{ID: store.ID(), Name: "fixture", Image: "fixture", Branch: "main",
		CPU: 500, Memory: 512, Desired: state, Generation: 1}
	op, err := db.Create(ctx, w, "", "", keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishOp(ctx, op, state, &store.Container{ID: "fixture-container", Generation: 1, ImageID: "fixture", Status: state}, ""); err != nil {
		t.Fatal(err)
	}
	return w
}

type inspectedContainer struct {
	runtime.Engine
	container runtime.Container
}

type failedInitEngine struct {
	runtime.Engine
	removed chan runtime.Spec
}

func (e failedInitEngine) Ensure(context.Context, runtime.Spec) (runtime.Container, error) {
	return runtime.Container{ID: "failed-candidate", State: "created"}, nil
}

func (e failedInitEngine) Start(context.Context, string) error { return nil }

func (e failedInitEngine) Run(context.Context, string, runtime.Exec) (int, error) {
	return 128, nil
}

func (e failedInitEngine) Remove(_ context.Context, spec runtime.Spec) error {
	e.removed <- spec
	return nil
}

func runReviewWorker(t *testing.T, c *control.Control) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func TestCreateInitializationFailureRemovesCandidate(t *testing.T) {
	db := newDB(t)
	engine := failedInitEngine{removed: make(chan runtime.Spec, 1)}
	c := &control.Control{Store: db, Engine: engine}
	a := newTestAPI(t, c)
	a.login()
	var created acceptance
	a.call("POST", "/workspaces", map[string]any{"name": "initialization failure", "auto_start": false}, 202, &created)
	runReviewWorker(t, c)
	var op store.Operation
	waitFor(t, "initialization failure", func() bool {
		a.call("GET", "/operations/"+created.Op, nil, 200, &op)
		return op.Status == "failed"
	})
	if !strings.Contains(op.Error, "initialize workspace:") {
		t.Fatalf("initialization error was not retained: %+v", op)
	}
	select {
	case candidate := <-engine.removed:
		if candidate.WorkspaceID != created.Workspace || candidate.Generation != 1 {
			t.Fatalf("removed wrong candidate: %+v", candidate)
		}
	default:
		t.Fatal("failed initialization left the candidate running")
	}
}

func (i inspectedContainer) Inspect(context.Context, runtime.Spec) (runtime.Container, error) {
	return i.container, nil
}

func TestStoppedWorkspaceAccessRequiresRebuildAfterSSHKeyChange(t *testing.T) {
	db := newDB(t)
	const network = "test-bridge"
	keys := []string{"fixture new public key"}
	w := seedWorkspace(t, db, "stopped", keys)
	info := runtime.Container{ID: "fixture-container", State: "exited",
		SSHRevision: remote.Revision(network, []string{"fixture previous public key"})}
	c := &control.Control{Store: db, SSH: remote.Config{Network: network}, Engine: inspectedContainer{container: info}}
	a := newTestAPI(t, c)
	a.login()
	var access control.Access
	a.call("GET", "/workspaces/"+w.ID+"/access", nil, 200, &access)
	if access.Status != "needs_rebuild" {
		t.Fatalf("access status = %q, want needs_rebuild", access.Status)
	}
	path := "/workspaces/" + w.ID
	var before store.Workspace
	a.call("GET", path, nil, 200, &before)
	var started acceptance
	a.call("POST", path+"/start", nil, 202, &started)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	var op store.Operation
	waitFor(t, "start refusal", func() bool {
		a.call("GET", "/operations/"+started.Op, nil, 200, &op)
		return op.Status == "failed" || op.Status == "succeeded"
	})
	if op.Status != "failed" || !strings.Contains(op.Error, "rebuild before starting") {
		t.Fatalf("start did not report that SSH settings require rebuilding: %+v", op)
	}
	var after store.Workspace
	a.call("GET", path, nil, 200, &after)
	if after != before {
		t.Fatalf("start refusal changed Workspace: before %+v, after %+v", before, after)
	}
}

func TestDeletedWorkspaceRemainsAvailableToOperationHistory(t *testing.T) {
	db, ctx := newDB(t), t.Context()
	w := seedWorkspace(t, db, "running", nil)
	op, err := db.Enqueue(ctx, w.ID, "delete", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishOp(ctx, op, "deleted", nil, ""); err != nil {
		t.Fatal(err)
	}

	a := newTestAPI(t, &control.Control{Store: db})
	a.login()
	var workspaces []store.Workspace
	a.call("GET", "/workspaces", nil, 200, &workspaces)
	if len(workspaces) != 0 {
		t.Fatalf("deleted workspace still in active workspace list: %+v", workspaces)
	}
	var operations []store.Operation
	a.call("GET", "/workspaces/"+w.ID+"/operations", nil, 200, &operations)
	if len(operations) != 2 || operations[0].ID != op.ID || operations[0].Status != "succeeded" || operations[1].Type != "create" {
		t.Fatalf("missing lifecycle history for deleted workspace: %+v", operations)
	}
}
