package agentlink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const heartbeatInterval = 5 * time.Second
const heartbeatTimeout = 20 * time.Second
const setupTimeout = 8 * time.Second

// Hub dispatches calls to authenticated Host Agents. Call IDs are one-use
// capabilities bound to the authenticated Host's current control connection.
type Hub struct {
	onReady      func(context.Context, string, runtime.Engine)
	authenticate func(context.Context) (string, error)
	heartbeat    func(context.Context, string, Inventory, ConfigureAgent) error
	disconnected func(context.Context, string)
	mu           sync.Mutex
	sessions     map[string]*session
	revocation   uint64
	revokedAt    map[string]uint64
}

// ConfigureAgent changes the configuration of the session that reported a heartbeat.
// Call it asynchronously to let heartbeat processing continue during the exchange.
type ConfigureAgent func(context.Context, AgentConfig) error

func NewHub(authenticate func(context.Context) (string, error), heartbeat func(context.Context, string, Inventory, ConfigureAgent) error, disconnected func(context.Context, string), onReady func(context.Context, string, runtime.Engine)) *Hub {
	return &Hub{
		authenticate: authenticate, heartbeat: heartbeat, disconnected: disconnected, onReady: onReady,
		sessions: make(map[string]*session), revokedAt: make(map[string]uint64),
	}
}

func (h *Hub) Register(registrar grpc.ServiceRegistrar) { registrar.RegisterService(&service, h) }

// Disconnect invalidates the current control stream and its data streams.
func (h *Hub) Disconnect(hostID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revocation++
	h.revokedAt[hostID] = h.revocation
	if current := h.sessions[hostID]; current != nil {
		current.cancel()
	}
}

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	lease   *sync.Mutex
	pending map[string]chan *exchange
	control *exchange
}

func (h *Hub) connect(s grpc.ServerStream) error {
	streamCtx := s.Context()
	h.mu.Lock()
	revocation := h.revocation
	h.mu.Unlock()
	hostID, err := h.authenticate(streamCtx)
	if err != nil || hostID == "" {
		return status.Error(codes.Unauthenticated, "host certificate rejected")
	}
	h.mu.Lock()
	// Identity replacement can occur while authentication reads the registry.
	// A connection admitted before revocation must leave its replacement alone.
	if h.revokedAt[hostID] > revocation {
		h.mu.Unlock()
		return status.Error(codes.Unauthenticated, "host certificate rejected")
	}
	if err := streamCtx.Err(); err != nil {
		h.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(streamCtx)
	control := newExchange(ctx, s)
	current := &session{ctx: ctx, cancel: cancel, lease: &sync.Mutex{}, pending: make(map[string]chan *exchange), control: control}
	if previous := h.sessions[hostID]; previous != nil {
		current.lease = previous.lease
		previous.cancel()
	}
	h.sessions[hostID] = current
	h.mu.Unlock()
	var readyCallbacks sync.WaitGroup
	defer func() {
		cancel()
		readyCallbacks.Wait()
		// Finish expiring the old lease before a replacement can refresh it.
		// The registry stays available to other Hosts during the write.
		current.lease.Lock()
		defer current.lease.Unlock()
		h.mu.Lock()
		ended := h.sessions[hostID] == current
		h.mu.Unlock()
		if ended && h.disconnected != nil {
			record, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer stop()
			h.disconnected(record, hostID)
		}
		h.mu.Lock()
		if h.sessions[hostID] == current {
			delete(h.sessions, hostID)
		}
		h.mu.Unlock()
	}()
	bound := &engine{hub: h, host: hostID, session: current}
	wasHealthy := false
	for ctx.Err() == nil {
		next, stop := context.WithTimeout(ctx, heartbeatTimeout)
		f, err := control.recv(next)
		stop()
		if err != nil {
			return status.Error(codes.Unavailable, "host heartbeat stream ended")
		}
		inventory, err := decode[Inventory](f, "heartbeat")
		if err != nil || inventory.MaxConcurrent < 1 || inventory.Host.CPUs < 0 || inventory.Host.Memory < 0 {
			return status.Error(codes.InvalidArgument, "invalid host heartbeat")
		}
		// Recheck against the live registry. Disabled or replaced identities
		// must not be able to refresh their lease.
		if id, e := h.authenticate(ctx); e != nil || id != hostID {
			return status.Error(codes.PermissionDenied, "host certificate rejected")
		}
		current.lease.Lock()
		if ctx.Err() == nil {
			err = h.heartbeat(ctx, hostID, inventory, bound.configure)
		}
		current.lease.Unlock()
		if err != nil {
			return status.Error(codes.Unavailable, "host heartbeat could not be recorded")
		}
		healthy := inventory.Error == ""
		if healthy && !wasHealthy && h.onReady != nil {
			// The callback can use the bound Engine while heartbeats continue.
			readyCallbacks.Go(func() { h.onReady(ctx, hostID, bound) })
		}
		wasHealthy = healthy
	}
	return nil
}

func (h *Hub) call(s grpc.ServerStream) error {
	hostID, err := h.authenticate(s.Context())
	if err != nil || hostID == "" {
		return status.Error(codes.Unauthenticated, "host certificate rejected")
	}
	ctx, cancel := context.WithCancel(s.Context())
	defer cancel()
	peer := newExchange(ctx, s)
	binding, stop := context.WithTimeout(ctx, setupTimeout)
	first, err := peer.recv(binding)
	stop()
	if err != nil {
		return status.Error(codes.DeadlineExceeded, "exchange did not identify a call")
	}
	id, err := decode[string](first, "bind")
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid exchange binding")
	}
	h.mu.Lock()
	current := h.sessions[hostID]
	var ready chan *exchange
	if current != nil && current.ctx.Err() == nil {
		ready = current.pending[id]
		delete(current.pending, id)
	}
	h.mu.Unlock()
	if ready == nil {
		return status.Error(codes.PermissionDenied, "call does not belong to this host session")
	}
	ready <- peer
	select {
	case <-current.ctx.Done():
	case <-peer.ctx.Done():
	}
	return nil
}

func (h *Hub) callAgent(ctx, setup context.Context, hostID string, expected *session, req request) (*exchange, error) {
	var random [16]byte
	rand.Read(random[:])
	id := hex.EncodeToString(random[:])
	ready := make(chan *exchange, 1)
	h.mu.Lock()
	current := h.sessions[hostID]
	if current == nil || current.ctx.Err() != nil || (expected != nil && current != expected) {
		h.mu.Unlock()
		return nil, runtime.ErrHostUnavailable
	}
	current.pending[id] = ready
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(current.pending, id)
		h.mu.Unlock()
	}()
	if err := current.control.putValue(setup, "dispatch", id); err != nil {
		return nil, err
	}
	select {
	case peer := <-ready:
		stopWatching := context.AfterFunc(ctx, peer.cancel)
		context.AfterFunc(peer.ctx, func() { stopWatching() })
		if err := peer.putValue(setup, "request", req); err != nil {
			peer.cancel()
			return nil, err
		}
		return peer, nil
	case <-current.ctx.Done():
		return nil, runtime.ErrHostUnavailable
	case <-setup.Done():
		return nil, unavailable(setup.Err())
	}
}

type outbound struct {
	frame frame
	sent  chan error
}

// exchange pumps at most one sender and one receiver per gRPC stream, so
// application reads, writes and cancellation remain independent.
type exchange struct {
	ctx       context.Context
	cancel    context.CancelFunc
	incoming  chan frame
	outgoing  chan outbound
	readDone  chan struct{}
	writeDone chan struct{}
	ended     error // why the stream stopped; written before incoming closes
}

func newExchange(parent context.Context, s stream) *exchange {
	ctx, cancel := context.WithCancel(parent)
	e := &exchange{ctx: ctx, cancel: cancel, incoming: make(chan frame, 1), outgoing: make(chan outbound), readDone: make(chan struct{}), writeDone: make(chan struct{})}
	go func() {
		defer close(e.readDone)
		defer close(e.incoming)
		for ctx.Err() == nil {
			f, err := receive(s)
			if err != nil {
				e.ended = err
				return
			}
			select {
			case e.incoming <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(e.writeDone)
		for ctx.Err() == nil {
			select {
			case out := <-e.outgoing:
				err := send(s, out.frame)
				out.sent <- err
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return e
}

func (e *exchange) recv(ctx context.Context) (frame, error) {
	select {
	case f, ok := <-e.incoming:
		if !ok {
			// Keep the peer's reason, such as a rejected Agent certificate.
			if e.ended != nil {
				return frame{}, unavailable(e.ended)
			}
			return frame{}, runtime.ErrHostUnavailable
		}
		return f, nil
	case <-e.ctx.Done():
		return frame{}, unavailable(e.ctx.Err())
	case <-ctx.Done():
		return frame{}, unavailable(ctx.Err())
	}
}

func (e *exchange) put(ctx context.Context, f frame) error {
	sent := make(chan error, 1)
	select {
	case e.outgoing <- outbound{frame: f, sent: sent}:
	case <-e.readDone:
		return runtime.ErrHostUnavailable
	case <-e.writeDone:
		return runtime.ErrHostUnavailable
	case <-e.ctx.Done():
		return unavailable(e.ctx.Err())
	case <-ctx.Done():
		return unavailable(ctx.Err())
	}
	select {
	case err := <-sent:
		return unavailable(err)
	case <-e.ctx.Done():
		return unavailable(e.ctx.Err())
	case <-ctx.Done():
		return unavailable(ctx.Err())
	}
}

func (e *exchange) putValue(ctx context.Context, kind string, value any) error {
	f, err := valueFrame(kind, value)
	if err != nil {
		return err
	}
	return e.put(ctx, f)
}

func unavailable(err error) error {
	if err == nil || errors.Is(err, runtime.ErrHostUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", runtime.ErrHostUnavailable, err)
}
