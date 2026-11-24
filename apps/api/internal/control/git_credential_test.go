package control

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type gitLifecycleEngine struct {
	*lifecycleEngine
	apply func(runtime.Spec, string, runtime.GitCredential) error
	init  func(runtime.Exec)
}

func (e *gitLifecycleEngine) ApplyGitCredential(_ context.Context, spec runtime.Spec, id string, credential runtime.GitCredential) error {
	return e.apply(spec, id, credential)
}

func (e *gitLifecycleEngine) Run(_ context.Context, _ string, exec runtime.Exec) (int, error) {
	e.init(exec)
	return 0, nil
}

func TestLifecycleWritesGitCredentialBeforeFirstStart(t *testing.T) {
	const repo = "https://git.example.test:8443/group/project"
	const branch = "feature/fixture"
	credential := store.RepositoryCredential{Host: "git.example.test:8443", Username: "fixture", Token: "synthetic-git-token"}
	for _, tc := range []struct {
		name, kind string
		configured bool
		wantWrite  bool
		writeError bool
	}{
		{name: "create", kind: "create", configured: true, wantWrite: true},
		{name: "rebuild", kind: "rebuild", configured: true, wantWrite: true},
		{name: "reset", kind: "reset", configured: true, wantWrite: true},
		{name: "stop and start", kind: "stop", configured: true},
		{name: "without credentials", kind: "rebuild"},
		{name: "write failure stops the operation", kind: "create", configured: true, wantWrite: true, writeError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, w, current := seedRecoveryWorkspace(t, "running", nil)
			engine := &gitLifecycleEngine{lifecycleEngine: &lifecycleEngine{containers: map[int]*runtime.Container{1: &current}}}
			c := &Control{Store: db, Engine: engine}
			target := w.Generation + 1
			var create store.Operation
			if tc.kind == "create" {
				w.ID, w.Name, w.RepoURL, w.Branch = store.ID(), "private", repo, branch
				var err error
				create, err = db.Create(ctx, w, "", "", nil, &credential)
				if err != nil {
					t.Fatal(err)
				}
				delete(engine.containers, 1)
				target = 1
			} else if tc.configured {
				if err := db.SetRepositoryCredential(ctx, w.ID, &credential); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			engine.apply = func(spec runtime.Spec, id string, got runtime.GitCredential) error {
				writes++
				if spec.WorkspaceID != w.ID || spec.Generation != target || engine.containers[target].ID != id ||
					engine.containers[target].State != "created" || engine.starts != 0 || got != runtime.GitCredential(credential) {
					t.Fatal("credential was changed, written to the wrong container, or written after Start")
				}
				if tc.writeError {
					return errors.New(credential.Token)
				}
				return nil
			}
			initialized := false
			engine.init = func(exec runtime.Exec) {
				initialized = true
				if tc.kind == "create" && !slices.Equal(exec.Args, []string{"workspace-control", "init", repo, branch}) {
					t.Fatalf("init did not receive the repository URL and branch: %v", exec.Args)
				}
			}
			if tc.kind == "create" {
				if !c.operation(ctx) {
					t.Fatal("Create was not dispatched")
				}
				op, err := db.Operation(ctx, create.ID)
				if err != nil {
					t.Fatal(err)
				}
				if tc.writeError {
					if op.Status != "failed" || op.Error == "" || strings.Contains(op.Error, credential.Token) || engine.starts != 0 || initialized {
						t.Fatal("failed credential write exposed its token or allowed first start")
					}
				} else if op.Status != "succeeded" || !initialized || engine.starts != 1 {
					t.Fatalf("Create did not start and initialize: %+v", op)
				}
			} else {
				op := runOperation(t, c, w.ID, tc.kind, false)
				if op.Status != "succeeded" {
					t.Fatalf("lifecycle failed: %+v", op)
				}
				if tc.kind == "stop" {
					op := runOperation(t, c, w.ID, "start", false)
					if op.Status != "succeeded" || initialized || engine.starts != 1 {
						t.Fatalf("Stop and Start did not reuse the existing container: %+v", op)
					}
				}
			}
			if (tc.wantWrite && writes != 1) || (!tc.wantWrite && writes != 0) {
				t.Fatalf("unexpected number of credential writes: %d", writes)
			}
		})
	}
}
