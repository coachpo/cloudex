package control

import (
	"context"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type codexLifecycleEngine struct {
	*lifecycleEngine
	apply func(runtime.Spec, string, runtime.InitialCodexFiles) error
}

func (e *codexLifecycleEngine) WriteInitialCodexFiles(_ context.Context, target runtime.Spec, id string, config runtime.InitialCodexFiles) error {
	return e.apply(target, id, config)
}

func TestLifecycleWritesInitialCodexFilesBeforeFirstStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         string
		hasDefaults  bool
		targetExists bool
		wantWrite    bool
	}{
		{name: "Create", kind: "create", hasDefaults: true, wantWrite: true},
		{name: "Rebuild", kind: "rebuild", hasDefaults: true},
		{name: "Reset", kind: "reset", hasDefaults: true},
		{name: "Start", kind: "start", hasDefaults: true},
		{name: "Create without defaults", kind: "create"},
		{name: "retry after first start", kind: "create", hasDefaults: true, targetExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, w, current := seedRecoveryWorkspace(t, "stopped", nil)
			settings := store.InitialCodexFiles{
				AgentsMD: "# Workspace guidance\n", ConfigTOML: "# Exact content\nmodel = 'fixture'\n",
			}
			if tc.hasDefaults {
				if err := db.SetInitialCodexFiles(ctx, settings); err != nil {
					t.Fatal(err)
				}
			}
			engine := &codexLifecycleEngine{lifecycleEngine: &lifecycleEngine{
				containers: map[int]*runtime.Container{1: &current},
			}}
			target := 2
			switch tc.kind {
			case "create":
				w.Generation = 0
				delete(engine.containers, 1)
				target = 1
			case "start":
				target = 1
			}
			if tc.targetExists {
				engine.containers[target] = &runtime.Container{ID: "previously-started", State: "exited"}
			}
			writes := 0
			engine.apply = func(spec runtime.Spec, id string, config runtime.InitialCodexFiles) error {
				writes++
				if spec.WorkspaceID != w.ID || spec.Generation != target || engine.containers[target].ID != id ||
					engine.containers[target].State != "created" || engine.starts != 0 {
					t.Fatalf("write reached wrong container or followed Start: %+v, %q", spec, id)
				}
				want := runtime.InitialCodexFiles(settings)
				if config != want {
					t.Fatalf("config changed before writing: %+v", config)
				}
				return nil
			}
			control := &Control{Store: db, Engine: engine}
			op := store.Operation{ID: "operation", Type: tc.kind, Target: target}
			if tc.kind == "reset" {
				if _, err := db.Enqueue(ctx, w.ID, tc.kind, false, "", ""); err != nil {
					t.Fatal(err)
				}
				claimed, _, err := db.ClaimOp(ctx)
				if err != nil {
					t.Fatal(err)
				}
				op = claimed
			}
			_, _, err := control.lifecycle(ctx, w, op)
			if err != nil || engine.starts != 1 {
				t.Fatalf("%s did not start: %v (starts=%d)", tc.kind, err, engine.starts)
			}
			wantWrites := 0
			if tc.wantWrite {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("wrote initial files %d times, want %d", writes, wantWrites)
			}
		})
	}
}
