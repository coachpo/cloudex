package control

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

// lifecycleEngine stands in for Docker and records the calls that decide
// whether a container was started and which volumes were removed.
type lifecycleEngine struct {
	runtime.Engine
	containers map[int]*runtime.Container
	starts     int
	purged     []string
	inspectErr error // returned by the next Inspect only
	stop       func(*runtime.Container) error
	ensure     func() error
}

func (e *lifecycleEngine) Inspect(_ context.Context, s runtime.Spec) (runtime.Container, error) {
	if err := e.inspectErr; err != nil {
		e.inspectErr = nil
		return runtime.Container{}, err
	}
	if c := e.containers[s.Generation]; c != nil {
		return *c, nil
	}
	return runtime.Container{}, runtime.ErrMissing
}

func (e *lifecycleEngine) Ensure(_ context.Context, s runtime.Spec) (runtime.Container, error) {
	if e.ensure != nil {
		if err := e.ensure(); err != nil {
			return runtime.Container{}, err
		}
	}
	if e.containers[s.Generation] == nil {
		e.containers[s.Generation] = &runtime.Container{
			ID: fmt.Sprintf("generation-%d", s.Generation), ImageID: "fixture", State: "created",
			Labels: map[string]string{runtime.CreationOperationLabel: s.CreationOperationID},
		}
	}
	return *e.containers[s.Generation], nil
}

func (e *lifecycleEngine) container(id string) *runtime.Container {
	for _, c := range e.containers {
		if c.ID == id {
			return c
		}
	}
	panic("lifecycle used an unknown container: " + id)
}

func (e *lifecycleEngine) SyncAuthorizedKeys(context.Context, string, []string) error { return nil }
func (e *lifecycleEngine) SSHHostKey(context.Context, string) (string, error) {
	return "fixture-host-key", nil
}

func (e *lifecycleEngine) Start(_ context.Context, id string) error {
	e.starts++
	e.container(id).State = "running"
	return nil
}

func (e *lifecycleEngine) Stop(_ context.Context, id string) error {
	if e.stop != nil {
		return e.stop(e.container(id))
	}
	e.container(id).State = "exited"
	return nil
}

func (e *lifecycleEngine) Remove(_ context.Context, s runtime.Spec) error {
	delete(e.containers, s.Generation)
	return nil
}

func (e *lifecycleEngine) Purge(_ context.Context, s runtime.Spec, purgeAll bool) error {
	if purgeAll {
		e.purged = append(e.purged, "all volumes")
	} else {
		e.purged = append(e.purged, "project volume")
	}
	return nil
}

func (e *lifecycleEngine) Run(context.Context, string, runtime.Exec) (int, error) { return 0, nil }

func runOperation(t *testing.T, c *Control, id, kind string, purge bool) store.Operation {
	t.Helper()
	ctx := t.Context()
	op, err := c.Store.Enqueue(ctx, id, kind, purge, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.operation(ctx) {
		t.Fatalf("%s was not processed", kind)
	}
	if op, err = c.Store.Operation(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestStartThatNeverReachedDockerDoesNotBreakWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		missing  bool
		observed string
	}{
		{name: "container could not be read", observed: "stopped"},
		{name: "container is gone", missing: true, observed: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, w, ci := seedRecoveryWorkspace(t, "stopped", nil)
			engine := &lifecycleEngine{containers: map[int]*runtime.Container{1: &ci}, inspectErr: errors.New("Docker is unavailable")}
			if tc.missing {
				engine.containers, engine.inspectErr = map[int]*runtime.Container{}, nil
			}
			c := &Control{Store: db, Engine: engine}
			op := runOperation(t, c, w.ID, "start", false)
			ws, err := db.Workspace(ctx, w.ID)
			if err != nil || op.Status != "failed" || op.Error == "" || engine.starts != 0 ||
				ws.Observed != "stopped" || ws.Desired != "stopped" || ws.Error != "" {
				t.Fatalf("unsent Start changed the Workspace: operation=%+v starts=%d workspace=%+v err=%v", op, engine.starts, ws, err)
			}
			// Reconciliation, not the failed operation, reports a container that is really gone.
			reconcileOnce(ctx, c)
			if ws, err = db.Workspace(ctx, w.ID); err != nil || ws.Observed != tc.observed {
				t.Fatalf("reconciliation after the failed Start: %+v %v", ws, err)
			}
			if tc.missing {
				return
			}
			if op = runOperation(t, c, w.ID, "start", false); op.Status != "succeeded" || engine.starts != 1 {
				t.Fatalf("Start could not be retried: %+v starts=%d", op, engine.starts)
			}
		})
	}
}

func TestFailedStopIsLeftToReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stopped  bool
		observed string
	}{
		{name: "container keeps running", observed: "running"},
		{name: "container stopped but the response was lost", stopped: true, observed: "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, w, ci := seedRecoveryWorkspace(t, "running", nil)
			engine := &lifecycleEngine{containers: map[int]*runtime.Container{1: &ci}, stop: func(c *runtime.Container) error {
				if tc.stopped {
					c.State = "exited"
				}
				return errors.New("Docker did not confirm the stop")
			}}
			c := &Control{Store: db, Engine: engine}
			op := runOperation(t, c, w.ID, "stop", false)
			reconcileOnce(ctx, c)
			ws, err := db.Workspace(ctx, w.ID)
			if err != nil || op.Status != "failed" || op.Error == "" ||
				ws.Observed != tc.observed || ws.Desired != "stopped" || ws.Error != "" {
				t.Fatalf("failed Stop did not follow Docker's actual state: operation=%+v workspace=%+v err=%v", op, ws, err)
			}
			if tc.stopped {
				return
			}
			if _, _, err := db.ReadyContainer(ctx, w.ID); err != nil {
				t.Fatalf("running Workspace refused interactive access after a failed Stop: %v", err)
			}
		})
	}
}

func TestLifecycleRemovesVolumesOnlyForResetAndPurge(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		purge  bool
		purged []string
	}{
		{kind: "stop"},
		{kind: "rebuild"},
		{kind: "delete"},
		{kind: "reset", purged: []string{"project volume"}},
		{kind: "delete", purge: true, purged: []string{"all volumes"}},
	} {
		t.Run(fmt.Sprintf("%s purge=%v", tc.kind, tc.purge), func(t *testing.T) {
			db, w, ci := seedRecoveryWorkspace(t, "running", nil)
			engine := &lifecycleEngine{containers: map[int]*runtime.Container{1: &ci}}
			op := runOperation(t, &Control{Store: db, Engine: engine}, w.ID, tc.kind, tc.purge)
			if op.Status != "succeeded" || !slices.Equal(engine.purged, tc.purged) {
				t.Fatalf("%s removed %v, want %v: %+v", tc.kind, engine.purged, tc.purged, op)
			}
		})
	}
}

func TestRestartedResetDoesNotPurgeTheNewProjectVolume(t *testing.T) {
	ctx := t.Context()
	db, w, ci := seedRecoveryWorkspace(t, "running", nil)
	engine := &lifecycleEngine{containers: map[int]*runtime.Container{1: &ci}}
	c := &Control{Store: db, Engine: engine}
	op, err := db.Enqueue(ctx, w.ID, "reset", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	worker, stop := context.WithCancel(ctx)
	defer stop()
	// The control plane exits after the purge, before the replacement exists.
	engine.ensure = func() error { stop(); return worker.Err() }
	c.operation(worker)
	pending, err := db.Operation(ctx, op.ID)
	if err != nil || pending.Status != "running" || !slices.Equal(engine.purged, []string{"project volume"}) {
		t.Fatalf("Reset did not reach the replay boundary: %+v purged=%v err=%v", pending, engine.purged, err)
	}
	if err := db.RequeueOperations(ctx); err != nil {
		t.Fatal(err)
	}
	engine.ensure = nil
	if !c.operation(ctx) {
		t.Fatal("restart did not replay the Reset")
	}
	done, err := db.Operation(ctx, op.ID)
	if err != nil || done.Status != "succeeded" || len(engine.purged) != 1 {
		t.Fatalf("replayed Reset purged the reinitialized project volume: %+v purged=%v err=%v", done, engine.purged, err)
	}
	ws, err := db.Workspace(ctx, w.ID)
	if err != nil || ws.Generation != op.Target || ws.Observed != "running" {
		t.Fatalf("replayed Reset did not finish on its original generation: %+v %v", ws, err)
	}
}

type delayedPurgeEngine struct {
	*lifecycleEngine
	projects  map[int]string
	delayNext func(func())
}

func (e *delayedPurgeEngine) Purge(ctx context.Context, s runtime.Spec, _ bool) error {
	remove := func() { delete(e.projects, s.ProjectVolumeRevision) }
	if delay := e.delayNext; delay != nil {
		e.delayNext = nil
		delay(remove)
		return runtime.ErrHostUnavailable
	}
	remove()
	return nil
}

func (e *delayedPurgeEngine) Ensure(ctx context.Context, s runtime.Spec) (runtime.Container, error) {
	if _, ok := e.projects[s.ProjectVolumeRevision]; !ok {
		e.projects[s.ProjectVolumeRevision] = "fresh project"
	}
	return e.lifecycleEngine.Ensure(ctx, s)
}

func TestDelayedResetPurgeCannotRemoveReplacement(t *testing.T) {
	ctx := t.Context()
	db, w, ci := seedRecoveryWorkspace(t, "running", nil)
	worker, stop := context.WithCancel(ctx)
	defer stop()
	var delayed func()
	engine := &delayedPurgeEngine{
		lifecycleEngine: &lifecycleEngine{containers: map[int]*runtime.Container{1: &ci}},
		projects:        map[int]string{0: "old project"},
		delayNext:       func(remove func()) { delayed = remove; stop() },
	}
	c := &Control{Store: db, Engine: engine}
	op, err := db.Enqueue(ctx, w.ID, "reset", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.operation(worker)
	if delayed == nil {
		t.Fatal("reset did not issue its purge")
	}
	if err := db.RequeueOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if !c.operation(ctx) {
		t.Fatal("restarted worker did not resume the reset")
	}
	done, err := db.Operation(ctx, op.ID)
	if err != nil || done.Status != "succeeded" {
		t.Fatalf("reset after disconnect: %+v %v", done, err)
	}
	delayed()
	reset, err := db.Workspace(ctx, w.ID)
	if err != nil || len(engine.projects) != 1 || engine.projects[reset.ProjectVolumeRevision] != "fresh project" {
		t.Fatalf("delayed Docker delete removed the replacement: workspace=%+v projects=%v err=%v", reset, engine.projects, err)
	}
	engine.projects[reset.ProjectVolumeRevision] = "user data"
	rebuilt := runOperation(t, c, w.ID, "rebuild", false)
	if rebuilt.Status != "succeeded" || len(engine.projects) != 1 || engine.projects[reset.ProjectVolumeRevision] != "user data" {
		t.Fatalf("rebuild changed the project volume: %+v %v", rebuilt, engine.projects)
	}
}
