package control

import (
	"bytes"
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/ssh"
)

type Control struct {
	Store  *store.Store
	Engine runtime.Engine
	Agents interface {
		Engine(hostID string) runtime.Engine
		DialSSH(context.Context, string, string, netip.AddrPort) (net.Conn, error)
	}
	Key             cipher.AEAD
	SSH             remote.Config
	GatewayKey      ssh.PublicKey
	GatewayKeyAlias string
	Gateway         GatewayLifecycle
}

func (c *Control) EngineFor(hostID string) runtime.Engine {
	if c.Agents != nil {
		return c.Agents.Engine(hostID)
	}
	return c.Engine
}

func (c *Control) Run(ctx context.Context) error {
	if e := c.Store.RequeueOperations(ctx); e != nil {
		return e
	}
	var workers sync.WaitGroup
	operationWorkers := 1
	if c.Agents != nil {
		operationWorkers = 16
	}
	for range operationWorkers {
		workers.Go(func() { c.loop(ctx, 300*time.Millisecond, c.operation) })
	}
	workers.Go(func() { c.runReconciliation(ctx) })
	workers.Wait()
	return nil
}

func (c *Control) loop(ctx context.Context, delay time.Duration, run func(context.Context) bool) {
	for ctx.Err() == nil {
		if run(ctx) {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func logError(ctx context.Context, what string, e error) {
	if e != nil && ctx.Err() == nil && !errors.Is(e, store.ErrMissing) {
		slog.Error(what, "error", e)
	}
}

func spec(w store.Workspace, gen int) runtime.Spec {
	return runtime.Spec{WorkspaceID: w.ID, Image: w.Image, Generation: gen, ProjectVolumeRevision: w.ProjectVolumeRevision, CPU: w.CPU, Memory: w.Memory}
}

var errStartUnverified = errors.New("verify started container")
var errResetCheckpoint = errors.New("persist project volume change")

// A rejection fails a Start or Stop without marking the Workspace as broken:
// Start was not sent to Docker, or reconciliation observes what Stop left.
type rejection struct{ error }

func (r rejection) Unwrap() error { return r.error }

// A Start with this phase may have reached Docker even if the worker exited.
const startMayHaveRun = 1

func (c *Control) operation(ctx context.Context) bool {
	o, w, e := c.Store.ClaimOp(ctx, c.Agents != nil)
	if e != nil {
		logError(ctx, "claim operation", e)
		return false
	}
	run, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	run = runtime.WithOperation(run, o.ID, w.ID)
	state, container, e := c.lifecycle(run, w, o)
	if ctx.Err() != nil {
		return false
	} // replay the same operation and generation on restart
	message := ""
	if e != nil {
		message = e.Error()
	}
	retry := errors.Is(e, errResetCheckpoint) || (c.Agents != nil && e != nil && (errors.Is(e, runtime.ErrHostUnavailable) || run.Err() != nil))
	cleanCreate := false
	if !retry && e != nil && o.Type == "create" {
		if verifier, ok := c.EngineFor(w.HostID).(interface {
			NoWorkspaceArtifacts(context.Context, runtime.Spec) (bool, error)
		}); ok {
			check, stop := context.WithTimeout(runtime.WithOperation(ctx, o.ID, w.ID), 10*time.Second)
			var verificationErr error
			cleanCreate, verificationErr = verifier.NoWorkspaceArtifacts(check, spec(w, o.Target))
			retry = errors.Is(verificationErr, runtime.ErrHostUnavailable) || check.Err() != nil
			stop()
		}
	}
	_, rejected := errors.AsType[rejection](e)
	if retry {
		slog.Warn("lifecycle outcome unconfirmed; operation requeued", "operation", o.ID, "type", o.Type, "host", w.HostID, "error", e)
	}
	for ctx.Err() == nil {
		var err error
		switch {
		case retry:
			err = c.Store.RetryOp(ctx, o.ID, "Waiting to confirm the operation outcome: "+message)
		case cleanCreate:
			err = c.Store.FailCleanCreate(ctx, o, message)
		case errors.Is(e, errStartUnverified):
			err = c.Store.FailUnverifiedStart(ctx, o, message)
		case rejected:
			err = c.Store.RejectOp(ctx, o.ID, message)
		default:
			err = c.Store.FinishOp(ctx, o, state, container, message)
		}
		if err == nil {
			if e == nil && state == "running" && c.Gateway != nil {
				logError(ctx, "prepare Workspace gateway credential", c.ReconcileGateway(ctx, w))
			}
			break
		}
		logError(ctx, "finish operation", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return true
}

func (c *Control) lifecycle(ctx context.Context, w store.Workspace, o store.Operation) (_ string, _ *store.Container, err error) {
	engine := c.EngineFor(w.HostID)
	startRecorded := o.Type == "start" && o.Phase == startMayHaveRun
	verifyStart := o.Type == "start" && (startRecorded || w.StartUnverified)
	replacing := o.Type == "rebuild" || o.Type == "reset"
	// Preserve the observation until this operation may have touched Docker
	// state, including a new replacement rejected by preflight.
	reject := o.Type == "start" || o.Type == "stop" || (replacing && w.Observed != "rebuilding")
	defer func() {
		switch {
		case err == nil:
		case verifyStart:
			err = fmt.Errorf("%w: %w", errStartUnverified, err)
		case reject:
			err = rejection{err}
		}
	}()
	target := spec(w, o.Target)
	target.SSHNetwork = c.SSH.Network
	if o.Type == "create" || replacing {
		target.CreationOperationID = o.ID
	}
	inspect := func() (runtime.Container, error) {
		if o.Type == "start" {
			return c.Inspect(ctx, w)
		}
		return engine.Inspect(ctx, target)
	}
	switch o.Type {
	case "delete":
		for _, generation := range []int{w.Generation, w.Generation + 1} {
			if generation > 0 {
				if e := engine.Remove(ctx, spec(w, generation)); e != nil {
					return "", nil, e
				}
			}
		}
		if o.Purge {
			if e := engine.Purge(ctx, target, true); e != nil {
				return "", nil, e
			}
		}
		return "deleted", nil, nil
	case "rebuild", "reset":
		if e := c.Store.BeginReplacement(ctx, o); e != nil {
			return "", nil, e
		}
		reject = false
		if w.Generation > 0 {
			if e := engine.Remove(ctx, spec(w, w.Generation)); e != nil {
				return "", nil, e
			}
		}
		if o.Type == "reset" && o.Phase == 0 {
			if e := engine.Remove(ctx, target); e != nil {
				return "", nil, e
			}
			if e := engine.Purge(ctx, target, false); e != nil {
				return "", nil, e
			}
			revision, e := c.Store.AdvanceResetVolume(ctx, o)
			if e != nil {
				return "", nil, fmt.Errorf("%w: %w", errResetCheckpoint, e)
			}
			target.ProjectVolumeRevision = revision
		}
	}
	var ci runtime.Container
	var e error
	switch o.Type {
	case "start", "stop":
		ci, e = inspect()
		if e != nil {
			return "", nil, e
		}
		if o.Type == "start" {
			if ci.State == "paused" {
				return "", nil, store.ErrWorkspacePaused
			}
			w.Desired = "running"
			e = c.syncContainerSSHKeys(ctx, engine, w.ID, ci.ID, func() error {
				if verifyStart && ci.State == "running" {
					return nil
				}
				if !startRecorded {
					if e := c.Store.OpPhase(ctx, o.ID, startMayHaveRun); e != nil {
						return e
					}
				}
				reject = false // Docker now decides the outcome.
				return engine.Start(ctx, ci.ID)
			})
			if e != nil {
				verifyStart = verifyStart || errors.Is(e, runtime.ErrStartOutcomeUnknown)
				return "", nil, e
			}
			verifyStart = true
			e = c.waitForSSH(ctx, engine, ci.ID)
		} else {
			e = engine.Stop(ctx, ci.ID)
		}
		if e != nil {
			return "", nil, e
		}
	case "create", "rebuild", "reset":
		ci, e = engine.Ensure(ctx, target)
		if e != nil {
			return "", nil, e
		}
		defer func() {
			if err == nil || (c.Agents != nil && (ctx.Err() != nil || errors.Is(err, runtime.ErrHostUnavailable))) {
				return
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			// The failed candidate is not recorded as the active generation.
			// Remove only its container; the named volumes belong to the Workspace.
			if e := engine.Remove(cleanup, target); e != nil {
				err = errors.Join(err, fmt.Errorf("remove failed container candidate: %w", e))
			}
		}()
		if !ci.CreatedByOperation(o.ID) {
			return "", nil, fmt.Errorf("%w: candidate creation operation does not match. Use a Host Agent built from the same revision as the control plane", runtime.ErrOwnership)
		}
		if ci.State == "created" {
			if e = c.applyGitCredential(ctx, engine, target, ci.ID); e != nil {
				return "", nil, e
			}
		}
		if e = c.syncContainerSSHKeys(ctx, engine, w.ID, ci.ID, func() error {
			if ci.State == "running" {
				return nil
			}
			return engine.Start(ctx, ci.ID)
		}); e != nil {
			return "", nil, e
		}
		if e = c.waitForSSH(ctx, engine, ci.ID); e != nil {
			return "", nil, e
		}
		var stderr capped
		code, e := engine.Run(ctx, ci.ID, runtime.Exec{Args: []string{"workspace-control", "init", w.RepoURL, w.Branch}, Dir: "/workspace", Stderr: &stderr})
		if e != nil {
			return "", nil, e
		}
		if code != 0 {
			return "", nil, fmt.Errorf("initialize workspace: %s (exit %d)", strings.TrimSpace(stderr.String()), code)
		}
		if w.Desired == "stopped" {
			if e = engine.Stop(ctx, ci.ID); e != nil {
				return "", nil, e
			}
		}
	default:
		return "", nil, errors.New("unknown lifecycle operation")
	}
	ci, e = inspect()
	if e != nil {
		return "", nil, e
	}
	state := "stopped"
	if ci.State == "running" {
		state = "running"
	}
	if state != w.Desired {
		return "", nil, fmt.Errorf("container is %s; expected %s", ci.State, w.Desired)
	}
	return state, &store.Container{ID: ci.ID, ImageID: ci.ImageID, Generation: o.Target, Status: ci.State}, nil
}

func (c *Control) Inspect(ctx context.Context, w store.Workspace) (runtime.Container, error) {
	if w.Generation == 0 {
		return runtime.Container{}, store.ErrMissing
	}
	current, e := c.EngineFor(w.HostID).Inspect(ctx, spec(w, w.Generation))
	if e != nil {
		return current, e
	}
	recorded, e := c.Store.Container(ctx, w.ID, w.Generation)
	if e != nil {
		return current, e
	}
	if current.ID != recorded.ID {
		return runtime.Container{}, runtime.ErrOwnership
	}
	return current, nil
}

func (c *Control) runReconciliation(ctx context.Context) {
	var hosts sync.WaitGroup
	defer hosts.Wait()
	var inFlight sync.Map
	c.loop(ctx, 5*time.Second, func(ctx context.Context) bool {
		c.reconcile(ctx, func(hostID string, reconcile func()) {
			// A slow batch must not hold back the next sweep of other Hosts.
			if _, busy := inFlight.LoadOrStore(hostID, struct{}{}); busy {
				return
			}
			hosts.Go(func() {
				defer inFlight.Delete(hostID)
				reconcile()
			})
		})
		return false
	})
}

func (c *Control) reconcile(ctx context.Context, schedule func(string, func())) {
	ws, e := c.Store.Workspaces(ctx)
	if e != nil {
		logError(ctx, "list for reconciliation", e)
		return
	}
	byHost := make(map[string][]store.Workspace)
	for _, w := range ws {
		byHost[w.HostID] = append(byHost[w.HostID], w)
	}
	for hostID, batch := range byHost {
		schedule(hostID, func() {
			for _, w := range batch {
				logError(ctx, "update observed state", c.reconcileWorkspace(ctx, w))
				if c.Gateway != nil {
					logError(ctx, "reconcile Workspace gateway credential", c.ReconcileGateway(ctx, w))
				}
			}
		})
	}
}

func (c *Control) reconcileWorkspace(ctx context.Context, w store.Workspace) error {
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if w.Observed != "running" && w.Observed != "stopped" && !w.StartUnverified {
			return nil
		}
		info, e := c.Inspect(check, w)
		if w.StartUnverified && e == nil && info.State == "running" && !c.readyAfterStart(check, w, info) {
			return nil
		}
		state, message := w.Observed, w.Error
		paused := info.State == "paused"
		startUnverified := false
		switch {
		case errors.Is(e, runtime.ErrMissing), errors.Is(e, runtime.ErrOwnership):
			state, message = "error", "Current container is missing or no longer matches the workspace. Rebuild to recover."
		case e != nil:
			return nil
		case info.State == "running":
			state, message = "running", ""
		case info.State == "exited" || info.State == "created":
			state, message = "stopped", ""
			if w.Desired == "running" {
				message = "Container stopped outside the control plane."
				// An unacknowledged Start may still take effect after this inspect.
				startUnverified = w.StartUnverified
			}
		case paused:
			state, message = "stopped", store.PausedObservation
			startUnverified = w.StartUnverified
		case info.State == "restarting":
			state, message = "stopped", "Docker reports the container is restarting."
			startUnverified = w.StartUnverified
		default:
			state, message = "error", "Container state: "+info.State
		}
		e = c.Store.Observe(check, w, state, message, paused, startUnverified)
		if !errors.Is(e, store.ErrObservationConflict) {
			return e
		}
		if attempt != 0 {
			break // Leave further conflicts to the next periodic reconciliation.
		}
		// Another sample won the CAS. Read both SQLite and Docker again.
		fresh, err := c.Store.Workspace(check, w.ID)
		if err != nil {
			return err
		}
		if fresh.Generation != w.Generation {
			return nil
		}
		w = fresh
	}
	return nil
}

func (c *Control) readyAfterStart(ctx context.Context, w store.Workspace, info runtime.Container) bool {
	if _, ok := c.gatewayRoute(info); !ok {
		return false
	}
	engine := c.EngineFor(w.HostID)
	if _, err := engine.SSHHostKey(ctx, info.ID); err != nil {
		return false
	}
	// A delayed Docker Start may have missed a key update while stopped.
	return c.syncContainerSSHKeys(ctx, engine, w.ID, info.ID, nil) == nil
}

const outputLimit = 256 * 1024

type capped struct {
	bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	remaining := outputLimit - c.Len()
	if len(p) > remaining {
		c.truncated = true
	}
	c.Buffer.Write(p[:min(remaining, len(p))])
	return len(p), nil
}
func ReadOutput(fn func(io.Writer) error) (string, bool, error) {
	var out capped
	e := fn(&out)
	return out.String(), out.truncated, e
}
