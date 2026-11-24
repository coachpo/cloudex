package agentlink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"google.golang.org/grpc"
)

// Agent executes the runtime vocabulary against the host's own Docker Engine.
type Agent struct {
	Engine            runtime.Engine
	Inventory         func(context.Context) (Inventory, error)
	HasImage          func(context.Context, string) (bool, error)
	ValidateContainer func(context.Context, string) error
	DialSSH           func(context.Context, string, netip.AddrPort) (net.Conn, error)
	JournalPath       string
	MaxConcurrent     int
}

func (a Agent) Run(ctx context.Context, conn grpc.ClientConnInterface) error {
	if a.Engine == nil || a.Inventory == nil {
		return errors.New("host agent requires an engine and inventory provider")
	}
	journal, err := openJournal(ctx, a.JournalPath)
	if err != nil {
		return err
	}
	if journal != nil {
		defer journal.Close()
	}
	execution := executor{Agent: a, ctx: ctx, journal: journal, gates: make(map[string]*workspaceGate),
		mutations: mutationSemaphore{limit: max(1, a.MaxConcurrent)}}
	defer execution.work.Wait()
	var reported string
	for ctx.Err() == nil {
		if err := execution.connect(ctx, conn); err != nil && ctx.Err() == nil {
			// A rejected certificate or unreachable control plane must be visible
			// at the default level; repeats of the same reason stay at Debug.
			if err.Error() != reported {
				reported = err.Error()
				slog.Warn("Host Agent link failed; reconnecting", "error", err)
			} else {
				slog.Debug("host agent reconnecting", "error", err)
			}
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

type executor struct {
	Agent
	ctx       context.Context
	journal   *journal
	work      sync.WaitGroup
	mu        sync.Mutex
	gates     map[string]*workspaceGate
	mutations mutationSemaphore

	configMu sync.Mutex
	images   []string
}

func (a *executor) connect(parent context.Context, conn grpc.ClientConnInterface) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stream, err := openStream(ctx, conn, controlStream)
	if err != nil {
		return err
	}
	peer := newExchange(ctx, stream)
	defer peer.cancel()
	beatEnded := make(chan struct{})
	go func() {
		defer close(beatEnded)
		defer cancel()
		_ = a.sendHeartbeats(ctx, peer)
	}()
	defer func() { cancel(); <-beatEnded }()
	for {
		f, err := peer.recv(ctx)
		if err != nil {
			return err
		}
		id, err := decode[string](f, "dispatch")
		if err != nil || len(id) != 32 {
			return errors.New("invalid runtime dispatch")
		}
		a.work.Go(func() { a.exchange(ctx, conn, id) })
	}
}

func (a *executor) sendHeartbeats(ctx context.Context, peer *exchange) error {
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for ctx.Err() == nil {
		probe, stop := context.WithTimeout(ctx, heartbeatInterval)
		inventory := a.heartbeatInventory(probe)
		stop()
		sending, sent := context.WithTimeout(ctx, heartbeatInterval)
		err := peer.putValue(sending, "heartbeat", inventory)
		sent()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return ctx.Err()
}

func (a *executor) heartbeatInventory(ctx context.Context) Inventory {
	a.configMu.Lock()
	images := a.images
	limit := a.mutations.Limit()
	a.configMu.Unlock()

	var checks sync.WaitGroup
	var inventory Inventory
	// Host info and image inspections share a heartbeat deadline. Run each
	// check independently so one Docker response cannot use another's time.
	checks.Go(func() {
		var err error
		inventory, err = a.Inventory(ctx)
		if err != nil {
			inventory.Error = err.Error()
		}
	})
	statuses := make([]ImageStatus, len(images))
	for i, image := range images {
		checks.Go(func() {
			ready, err := a.HasImage(ctx, image)
			statuses[i] = ImageStatus{Image: image, Ready: err == nil && ready}
		})
	}
	checks.Wait()
	inventory.MaxConcurrent = limit
	inventory.Images = statuses
	return inventory
}

func (a *executor) exchange(parent context.Context, conn grpc.ClientConnInterface, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stream, err := openStream(ctx, conn, callStream)
	if err != nil {
		return
	}
	peer := newExchange(ctx, stream)
	defer peer.cancel()
	setup, stop := context.WithTimeout(ctx, setupTimeout)
	if err := peer.putValue(setup, "bind", id); err != nil {
		stop()
		return
	}
	f, err := peer.recv(setup)
	stop()
	if err != nil {
		return
	}
	req, err := decode[request](f, "request")
	var value any
	if err == nil {
		// Closing the peer interrupts a streaming runtime operation. Accepted
		// lifecycle mutations use the agent's own deadline inside execute.
		workCtx, end := context.WithCancel(ctx)
		defer end()
		go func() {
			select {
			case <-peer.readDone:
				end()
			case <-workCtx.Done():
			}
		}()
		value, err = a.execute(workCtx, peer, req)
	}
	result, frameErr := valueFrame("result", value)
	if frameErr != nil {
		err = frameErr
	}
	result.Error = encodeError(runtimeOutcome(err))
	sending, sent := context.WithTimeout(ctx, setupTimeout)
	err = peer.put(sending, result)
	sent()
	if err != nil {
		return
	}
	// A successful SendMsg only queues the response. Let the server close
	// the stream after reading the result before cancelling our transport.
	ack, stop := context.WithTimeout(ctx, setupTimeout)
	defer stop()
	select {
	case <-peer.readDone:
	case <-ack.Done():
	}
}

// Docker connection loss does not establish the state of a remote mutation.
// A Docker error reply does, even a 5xx Start the control plane verifies later
// through ErrStartOutcomeUnknown, so it is not reported as a lost Host.
func runtimeOutcome(err error) error {
	var networkError net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &networkError) || errdefs.IsUnavailable(err) || client.IsErrConnectionFailed(err) {
		return unavailable(err)
	}
	return err
}

func (a *executor) execute(ctx context.Context, peer *exchange, req request) (any, error) {
	switch req.Method {
	case "configure":
		if req.Config == nil || req.Config.MaxConcurrent < 1 {
			return nil, errors.New("maximum concurrent operations must be positive")
		}
		if len(req.Config.Images) != 0 && a.HasImage == nil {
			return nil, errors.New("workspace image inspection is unavailable")
		}
		a.configMu.Lock()
		defer a.configMu.Unlock()
		if err := a.mutations.SetLimit(ctx, req.Config.MaxConcurrent); err != nil {
			return nil, err
		}
		if req.Config.Images != nil {
			// Heartbeats can probe the previous snapshot without holding the lock.
			a.images = slices.Clone(req.Config.Images)
		}
		return nil, nil
	case "ensure", "start", "stop", "remove", "purge", "apply-codex-config", "apply-git-credential", "sync-authorized-keys":
		return a.mutate(ctx, req)
	case "inspect", "artifacts-absent":
		if req.Spec == nil {
			return nil, errors.New("workspace specification required")
		}
		if req.Method == "artifacts-absent" {
			checker, ok := a.Engine.(interface {
				NoWorkspaceArtifacts(context.Context, runtime.Spec) (bool, error)
			})
			if !ok {
				return false, nil
			}
			unlock, err := a.lock(ctx, req.Spec.WorkspaceID)
			if err != nil {
				return false, err
			}
			defer unlock()
			return checker.NoWorkspaceArtifacts(ctx, *req.Spec)
		}
		c, err := a.Engine.Inspect(ctx, *req.Spec)
		if err == nil {
			if docker, ok := a.Engine.(interface {
				InspectDigests(context.Context, *runtime.Container)
			}); ok {
				docker.InspectDigests(ctx, &c)
			}
		}
		return encodeContainer(c), err
	case "host":
		return a.Engine.Host(ctx)
	case "containers":
		return a.Engine.Containers(ctx)
	case "images":
		return a.Engine.Images(ctx)
	case "run", "logs", "ssh-host-key", "terminal", "dial-ssh", "metrics":
		if err := a.validateContainer(ctx, req.ID); err != nil {
			return nil, err
		}
		return a.onContainer(ctx, peer, req)
	default:
		return nil, errors.New("unknown runtime request")
	}
}

func (a *executor) validateContainer(ctx context.Context, id string) error {
	if id == "" {
		return runtime.ErrOwnership
	}
	if a.ValidateContainer != nil {
		return a.ValidateContainer(ctx, id)
	}
	return nil
}

func (a *executor) onContainer(ctx context.Context, peer *exchange, req request) (any, error) {
	switch req.Method {
	case "run":
		if req.Exec == nil {
			return nil, errors.New("exec specification required")
		}
		x := runtime.Exec{Args: req.Exec.Args, Dir: req.Exec.Dir,
			Stdout: outputWriter{peer: peer, ctx: ctx, kind: "stdout"}, Stderr: outputWriter{peer: peer, ctx: ctx, kind: "stderr"}}
		return a.Engine.Run(ctx, req.ID, x)
	case "logs":
		return nil, a.Engine.Logs(ctx, req.ID, outputWriter{peer: peer, ctx: ctx, kind: "stdout"})
	case "ssh-host-key":
		return a.Engine.SSHHostKey(ctx, req.ID)
	case "terminal":
		opening, stop := context.WithTimeout(ctx, setupTimeout)
		tty, err := a.Engine.Terminal(opening, req.ID)
		stop()
		if err != nil {
			return nil, err
		}
		return nil, serveDuplex(ctx, peer, tty, tty.Resize)
	case "dial-ssh":
		if a.DialSSH == nil || !req.Address.IsValid() {
			return nil, errors.New("workspace SSH route unavailable")
		}
		opening, stop := context.WithTimeout(ctx, setupTimeout)
		socket, err := a.DialSSH(opening, req.ID, req.Address)
		stop()
		if err != nil {
			return nil, err
		}
		return nil, serveDuplex(ctx, peer, socket, nil)
	case "metrics":
		return a.Engine.Metrics(ctx, req.ID)
	default:
		return nil, errors.New("unknown container request")
	}
}

type outputWriter struct {
	peer *exchange
	ctx  context.Context
	kind string
}

func (w outputWriter) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		chunk := min(len(p), chunkSize)
		if err := w.peer.put(w.ctx, frame{Kind: w.kind, Data: append([]byte(nil), p[:chunk]...)}); err != nil {
			return n, err
		}
		n += chunk
		p = p[chunk:]
	}
	return n, nil
}
