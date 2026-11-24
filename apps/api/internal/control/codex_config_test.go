package control

import (
	"context"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type codexLifecycleEngine struct {
	*lifecycleEngine
	apply func(runtime.Spec, string, runtime.CodexConfig) error
}

func (e *codexLifecycleEngine) ApplyCodexConfig(_ context.Context, target runtime.Spec, id string, config runtime.CodexConfig) error {
	return e.apply(target, id, config)
}

func TestLifecycleWritesCodexConfigBeforeFirstStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         string
		managed      bool
		targetExists bool
		wantWrite    bool
	}{
		{name: "Create", kind: "create", managed: true, wantWrite: true},
		{name: "Rebuild", kind: "rebuild", managed: true, wantWrite: true},
		{name: "Reset", kind: "reset", managed: true, wantWrite: true},
		{name: "Start", kind: "start", managed: true},
		{name: "unmanaged", kind: "rebuild"},
		{name: "retry after first start", kind: "rebuild", managed: true, targetExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, w, current := seedRecoveryWorkspace(t, "stopped", nil)
			settings := store.CodexConfigSettings{
				Managed: tc.managed, AgentsMD: "# Workspace guidance\n",
				ConfigTOML: "# Exact content\nmodel = 'fixture'\n", AuthJSON: `{"fixture":"fake-login"}`,
			}
			saved, err := db.SaveCodexConfig(ctx, w.ID, 0, settings)
			if err != nil {
				t.Fatal(err)
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
			engine.apply = func(spec runtime.Spec, id string, config runtime.CodexConfig) error {
				writes++
				if spec.WorkspaceID != w.ID || spec.Generation != target || engine.containers[target].ID != id ||
					engine.containers[target].State != "created" || engine.starts != 0 {
					t.Fatalf("write reached wrong container or followed Start: %+v, %q", spec, id)
				}
				want := runtime.CodexConfig{AgentsMD: settings.AgentsMD, ConfigTOML: settings.ConfigTOML, AuthJSON: settings.AuthJSON}
				if config != want {
					t.Fatalf("config changed before writing: %+v", config)
				}
				return nil
			}
			control := &Control{Store: db, Engine: engine}
			_, _, err = control.lifecycle(ctx, w, store.Operation{ID: "operation", Type: tc.kind, Target: target})
			if err != nil || engine.starts != 1 {
				t.Fatalf("%s did not start: %v (starts=%d)", tc.kind, err, engine.starts)
			}
			applied, err := db.CodexConfig(ctx, w.ID)
			wantWrites, wantRevision := 0, int64(0)
			if tc.wantWrite {
				wantWrites, wantRevision = 1, saved.Revision
			}
			if err != nil || writes != wantWrites || applied.AppliedRevision != wantRevision {
				t.Fatalf("writes=%d applied_revision=%d error=%v", writes, applied.AppliedRevision, err)
			}
		})
	}
}
