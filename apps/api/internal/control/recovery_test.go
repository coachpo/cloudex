package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type recoveryEngine struct {
	runtime.Engine
	inspect func(context.Context) (runtime.Container, error)
	start   func() error
	run     func(context.Context, runtime.Exec) (int, error)
	hostKey func() (string, error)
}

func (e recoveryEngine) Inspect(ctx context.Context, _ runtime.Spec) (runtime.Container, error) {
	return e.inspect(ctx)
}

func (e recoveryEngine) Start(context.Context, string) error { return e.start() }

func (e recoveryEngine) Run(ctx context.Context, _ string, exec runtime.Exec) (int, error) {
	return e.run(ctx, exec)
}

func (e recoveryEngine) SyncAuthorizedKeys(context.Context, string, []string) error { return nil }

func (e recoveryEngine) SSHHostKey(context.Context, string) (string, error) {
	if e.hostKey != nil {
		return e.hostKey()
	}
	return "fixture-host-key", nil
}

type reconciliationAgents struct {
	engines map[string]runtime.Engine
}

func (a reconciliationAgents) Engine(hostID string) runtime.Engine { return a.engines[hostID] }

func (a reconciliationAgents) DialSSH(context.Context, string, string, netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("unexpected SSH dial")
}

func reconcileOnce(ctx context.Context, c *Control) {
	c.reconcile(ctx, func(_ string, reconcile func()) { reconcile() })
}

func TestSlowHostDoesNotDelayNextReconciliationOfOtherHosts(t *testing.T) {
	db, _, local := seedRecoveryWorkspace(t, "running", nil)
	ctx := t.Context()
	host, err := db.AddHost(ctx, "slow", "test-token", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnrollHost(ctx, host.ID, "test-token", "test-key"); err != nil {
		t.Fatal(err)
	}
	report := store.HostReport{Images: []store.ImageStatus{{Image: "fixture", Ready: true}}, Healthy: true, MaxOperations: 1,
		CPU: 4000, Memory: 4 << 30}
	if err := db.Heartbeat(ctx, host.ID, "test-key", report); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		w := store.Workspace{ID: store.ID(), HostID: host.ID, Name: fmt.Sprintf("slow %d", i),
			Image: "fixture", Branch: "main", Desired: "running", CPU: 500, Memory: 512}
		op, err := db.Create(ctx, w, "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishOp(ctx, op, "running", &store.Container{ID: w.ID, Generation: 1, Status: "running"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	var localPasses atomic.Int32
	var slowInFlight atomic.Int32
	localResampled := make(chan struct{})
	c := &Control{Store: db, Agents: reconciliationAgents{engines: map[string]runtime.Engine{
		store.LocalHostID: recoveryEngine{inspect: func(context.Context) (runtime.Container, error) {
			if localPasses.Add(1) == 2 {
				close(localResampled)
			}
			return local, nil
		}},
		host.ID: recoveryEngine{inspect: func(ctx context.Context) (runtime.Container, error) {
			if slowInFlight.Add(1) != 1 {
				t.Error("reconciliation overlapped on the slow Host")
			}
			defer slowInFlight.Add(-1)
			<-ctx.Done()
			return runtime.Container{}, ctx.Err()
		}},
	}}}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(run) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-localResampled:
	case <-time.After(8 * time.Second):
		t.Fatal("healthy Host was not inspected on the next reconciliation interval")
	}
}

func seedRecoveryWorkspace(t *testing.T, state string, keys []string) (*store.Store, store.Workspace, runtime.Container) {
	t.Helper()
	return seedRecoveryWorkspaceAt(t, filepath.Join(t.TempDir(), "test.sqlite"), state, keys)
}

func seedRecoveryWorkspaceAt(t *testing.T, path, state string, keys []string) (*store.Store, store.Workspace, runtime.Container) {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w := store.Workspace{ID: store.ID(), Name: "recovery", Image: "fixture", Branch: "main",
		CPU: 500, Memory: 512, Desired: state, Generation: 1}
	if err := db.SetSSHKeys(ctx, keys); err != nil {
		t.Fatal(err)
	}
	op, err := db.Create(ctx, w, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ci := runtime.Container{ID: "fixture-container", ImageID: "fixture", State: state, StartedAt: "fixture-original-start", NetworkIPs: map[string]netip.Addr{"": netip.MustParseAddr("172.19.0.2")}}
	if state == "stopped" {
		ci.State = "exited"
	}
	if err := db.FinishOp(ctx, op, state, &store.Container{ID: ci.ID, ImageID: ci.ImageID, Generation: 1, Status: ci.State}, ""); err != nil {
		t.Fatal(err)
	}
	return db, w, ci
}

func TestReconcileVerifiedStartWithoutReplacingContainer(t *testing.T) {
	for _, mode := range []string{"inspect fails", "start response lost", "start takes effect after first reconciliation"} {
		t.Run(mode, func(t *testing.T) { testReconcileVerifiedStart(t, mode) })
	}
}

func testReconcileVerifiedStart(t *testing.T, mode string) {
	t.Helper()
	const network = "fixture-network"
	keys := []string{"fixture-key"}
	ctx := t.Context()
	db, w, ci := seedRecoveryWorkspace(t, "stopped", keys)
	ci.NetworkIPs = map[string]netip.Addr{network: netip.MustParseAddr("172.19.0.2")}
	op, err := db.Enqueue(ctx, w.ID, "start", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var inspectFailure, sshUnavailable bool
	engine := recoveryEngine{
		inspect: func(context.Context) (runtime.Container, error) {
			if inspectFailure {
				inspectFailure, sshUnavailable = false, true
				return runtime.Container{}, errors.New("temporary inspect failure")
			}
			return ci, nil
		},
		start: func() error {
			switch mode {
			case "inspect fails":
				ci.State, inspectFailure = "running", true
				return nil
			case "start response lost":
				ci.State = "running"
			}
			sshUnavailable = true
			return fmt.Errorf("%w: %w", runtime.ErrStartOutcomeUnknown, io.EOF)
		},
		hostKey: func() (string, error) {
			if sshUnavailable {
				return "", errors.New("sshd is not ready")
			}
			return "fixture-host-key", nil
		},
	}
	c := &Control{Store: db, Engine: engine, SSH: remote.Config{Network: network}}
	if !c.operation(ctx) {
		t.Fatal("start operation not processed")
	}
	failed, err := db.Operation(ctx, op.ID)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("start probe failure not recorded: %+v %v", failed, err)
	}
	reconcileOnce(ctx, c)
	ws, err := db.Workspace(ctx, w.ID)
	expected := "error"
	if mode == "start takes effect after first reconciliation" {
		expected = "stopped"
	}
	if err != nil || ws.Observed != expected {
		t.Fatalf("unready Workspace was promoted: %+v %v", ws, err)
	}
	if mode == "start takes effect after first reconciliation" {
		ci.State = "running"
		reconcileOnce(ctx, c)
		ws, err = db.Workspace(ctx, w.ID)
		if err != nil || ws.Observed != "stopped" {
			t.Fatalf("late Start bypassed SSH verification: %+v %v", ws, err)
		}
	}
	sshUnavailable = false
	reconcileOnce(ctx, c)
	ws, err = db.Workspace(ctx, w.ID)
	if err != nil || ws.Observed != "running" || ws.Desired != "running" || ws.Error != "" || ws.Generation != w.Generation {
		t.Fatalf("healthy Workspace was not restored: %+v %v", ws, err)
	}
	if _, _, err := db.ReadyContainer(ctx, w.ID); err != nil {
		t.Fatalf("recovered Workspace did not accept interactive access: %v", err)
	}
	record, err := db.Container(ctx, w.ID, w.Generation)
	if err != nil || record.ID != ci.ID || record.Status != "running" || record.ImageID != ci.ImageID {
		t.Fatalf("verified Start did not update the original generation: %+v %v", record, err)
	}
}

func TestReplayStartAfterControllerRestart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstInspect error
	}{
		{name: "start already applied"},
		{name: "first inspection of replay times out", firstInspect: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "replay.sqlite")
			db, w, ci := seedRecoveryWorkspaceAt(t, path, "stopped", nil)
			op, err := db.Enqueue(ctx, w.ID, "start", false, "", "")
			if err != nil {
				t.Fatal(err)
			}
			worker, stop := context.WithCancel(ctx)
			defer stop()
			var starts int
			first := &Control{Store: db, Engine: recoveryEngine{
				inspect: func(context.Context) (runtime.Container, error) { return ci, nil },
				start: func() error {
					starts++
					ci.State = "running"
					stop() // Docker applied Start before the worker recorded its outcome.
					return worker.Err()
				},
			}}
			first.operation(worker)
			pending, err := db.Operation(ctx, op.ID)
			if err != nil || pending.Status != "running" || starts != 1 {
				t.Fatalf("Start did not reach the replay boundary: %+v starts=%d err=%v", pending, starts, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.RequeueOperations(ctx); err != nil {
				t.Fatal(err)
			}
			replayError := tc.firstInspect
			restarted := &Control{Store: reopened, Engine: recoveryEngine{
				inspect: func(context.Context) (runtime.Container, error) {
					if replayError != nil {
						failure := replayError
						replayError = nil
						return runtime.Container{}, failure
					}
					return ci, nil
				},
				start: func() error {
					starts++
					return errors.New("already applied Start must not be resent")
				},
			}}
			if !restarted.operation(ctx) {
				t.Fatal("restart did not process the persisted Start")
			}
			if tc.firstInspect != nil {
				waiting, err := reopened.Workspace(ctx, w.ID)
				if err != nil || waiting.Observed != "error" || !waiting.StartUnverified {
					t.Fatalf("failed replay lost recovery eligibility: %+v %v", waiting, err)
				}
			}
			reconcileOnce(ctx, restarted)
			current, err := reopened.Workspace(ctx, w.ID)
			if err != nil || current.Observed != "running" || current.Desired != "running" ||
				current.StartUnverified || current.Generation != w.Generation || starts != 1 {
				t.Fatalf("replay did not restore the original Workspace: %+v starts=%d err=%v", current, starts, err)
			}
			record, err := reopened.Container(ctx, w.ID, w.Generation)
			if err != nil || record.ID != ci.ID || record.Status != "running" {
				t.Fatalf("replay did not restore the original generation: %+v %v", record, err)
			}
		})
	}
}

func TestSecondStartPreservesEarlierUnverifiedStart(t *testing.T) {
	ctx := t.Context()
	db, w, ci := seedRecoveryWorkspace(t, "stopped", nil)
	var inspectErr error
	starts := 0
	c := &Control{Store: db, Engine: recoveryEngine{
		inspect: func(context.Context) (runtime.Container, error) { return ci, inspectErr },
		start: func() error {
			starts++
			return fmt.Errorf("%w: %w", runtime.ErrStartOutcomeUnknown, io.EOF)
		},
	}}
	if _, err := db.Enqueue(ctx, w.ID, "start", false, "", ""); err != nil {
		t.Fatal(err)
	}
	c.operation(ctx)
	reconcileOnce(ctx, c) // The first Start has not yet taken effect.
	waiting, err := db.Workspace(ctx, w.ID)
	if err != nil || waiting.Observed != "stopped" || !waiting.StartUnverified {
		t.Fatalf("first Start was not awaiting verification: %+v %v", waiting, err)
	}

	second, err := db.Enqueue(ctx, w.ID, "start", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	inspectErr = context.DeadlineExceeded
	c.operation(ctx)
	retry, err := db.Operation(ctx, second.ID)
	if err != nil || retry.Status != "failed" || starts != 1 {
		t.Fatalf("second Start must fail before sending another request: %+v starts=%d err=%v", retry, starts, err)
	}
	waiting, err = db.Workspace(ctx, w.ID)
	if err != nil || !waiting.StartUnverified {
		t.Fatalf("second Start discarded the first request's uncertainty: %+v %v", waiting, err)
	}

	inspectErr = nil
	ci.State = "running"
	reconcileOnce(ctx, c)
	ready, err := db.Workspace(ctx, w.ID)
	if err != nil || ready.Observed != "running" || ready.StartUnverified || ready.Generation != w.Generation {
		t.Fatalf("late first Start was not recovered: %+v %v", ready, err)
	}
	record, err := db.Container(ctx, w.ID, w.Generation)
	if err != nil || record.ID != ci.ID || record.Status != "running" {
		t.Fatalf("late first Start did not update the original generation: %+v %v", record, err)
	}
}

func TestReconcileResamplesDockerAfterPausedObservationWinsCAS(t *testing.T) {
	for _, latest := range []string{"running", "exited"} {
		t.Run(latest, func(t *testing.T) {
			ctx := t.Context()
			db, w, ci := seedRecoveryWorkspace(t, "running", nil)
			pauseSnapshot, err := db.Workspace(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			inspects := 0
			c := &Control{Store: db, Engine: recoveryEngine{
				inspect: func(context.Context) (runtime.Container, error) {
					inspects++
					sample := ci
					if inspects == 1 {
						// The reconciler has read running. The older pause sample
						// commits before that reading can be published.
						pending, err := db.Enqueue(ctx, w.ID, "stop", false, "", "")
						if err != nil {
							t.Fatal(err)
						}
						if err := db.CancelOp(ctx, pending.ID); err != nil {
							t.Fatal(err)
						}
						if err := db.Observe(ctx, pauseSnapshot, "stopped", store.PausedObservation, true, false); err != nil {
							t.Fatal(err)
						}
						paused, err := db.Workspace(ctx, w.ID)
						if err != nil || !paused.Paused {
							t.Fatalf("old pause did not win publication: %+v %v", paused, err)
						}
					} else {
						sample.State = latest
					}
					return sample, nil
				},
			}}
			reconcileOnce(ctx, c)
			if inspects != 2 {
				t.Fatalf("expected one fresh Docker inspection after CAS miss, got %d inspections", inspects)
			}
			state := "running"
			if latest == "exited" {
				state = "stopped"
			}
			current, err := db.Workspace(ctx, w.ID)
			if err != nil || current.Paused || current.Observed != state {
				t.Fatalf("reconciliation did not publish the fresh reading: %+v %v", current, err)
			}
		})
	}
}

func TestNoOpRunningObservationInvalidatesLatePause(t *testing.T) {
	ctx := t.Context()
	db, w, ci := seedRecoveryWorkspace(t, "running", nil)
	before, err := db.Workspace(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := &Control{Store: db}
	checked := false
	c.Engine = recoveryEngine{inspect: func(context.Context) (runtime.Container, error) {
		if !checked {
			checked = true
			old := ci
			old.State = "paused"
			reconcileOnce(ctx, c) // Newer running reading, before any pause has been persisted.
			return old, nil
		}
		return ci, nil
	}}
	reconcileOnce(ctx, c)
	after, err := db.Workspace(ctx, w.ID)
	if err != nil || after.Observed != "running" || after.Paused || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("late pause overwrote the no-op running observation: %+v %v", after, err)
	}
	if after.ObservationVersion != before.ObservationVersion+2 {
		t.Fatal("expected the no-op observation and the fresh reconciliation sample to publish")
	}
}

func TestReconcileDoesNotClearFailedRebuild(t *testing.T) {
	ctx := t.Context()
	db, w, ci := seedRecoveryWorkspace(t, "running", nil)
	if _, err := db.Enqueue(ctx, w.ID, "rebuild", false, "", ""); err != nil {
		t.Fatal(err)
	}
	op, _, err := db.ClaimOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const failure = "could not remove old container"
	if err := db.FinishOp(ctx, op, "", nil, failure); err != nil {
		t.Fatal(err)
	}
	c := &Control{Store: db, Engine: recoveryEngine{inspect: func(context.Context) (runtime.Container, error) { return ci, nil }}}
	reconcileOnce(ctx, c)
	ws, err := db.Workspace(ctx, w.ID)
	if err != nil || ws.Observed != "error" || ws.Error != failure {
		t.Fatalf("failed rebuild unexpectedly cleared: %+v %v", ws, err)
	}
}
