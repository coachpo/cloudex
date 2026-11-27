// Package hostmanager routes operations to each Host's active connection.
package hostmanager

import (
	"context"
	"crypto/cipher"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentlink"
	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type Manager struct {
	ctx               context.Context
	cancel            context.CancelFunc
	db                *store.Store
	instance, network string
	key               cipher.AEAD
	agents            *agentlink.Hub
	onReady           func(context.Context, string, runtime.Engine)

	mu    sync.Mutex
	hosts map[string]*worker
	jobs  sync.WaitGroup
}

type worker struct {
	cancel  context.CancelFunc
	done    chan struct{}
	session *session
	pollNow chan struct{}
}

// stop is called with the manager lock held so the route cannot change during shutdown.
func (w *worker) stop() {
	w.cancel()
	if w.session != nil {
		w.session.client.Close()
	}
}

type session struct {
	client *hostssh.Client
	docker *runtime.Docker
}

func (s *session) close() {
	s.client.Close()
	s.docker.Close()
}

func New(ctx context.Context, db *store.Store, instance, network string, key cipher.AEAD, agents *agentlink.Hub, onReady func(context.Context, string, runtime.Engine)) (*Manager, error) {
	hosts, err := db.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: ctx, cancel: cancel, db: db, instance: instance, network: network, key: key, agents: agents, onReady: onReady, hosts: make(map[string]*worker)}
	for _, host := range hosts {
		if host.ConnectionType == "ssh" {
			m.Changed(host.ID)
		}
	}
	return m, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.jobs.Wait()
}

// Changed revokes the previous route immediately. The replacement reads the
// committed configuration after its predecessor's final callback has returned.
func (m *Manager) Changed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restartLocked(id)
}

// Removed stops the SSH worker without replacing it after the registration is deleted.
func (m *Manager) Removed(id string) {
	m.mu.Lock()
	w := m.hosts[id]
	delete(m.hosts, id)
	if w != nil {
		w.stop()
	}
	m.mu.Unlock()
	if w != nil {
		<-w.done
	}
}

// Retry uses the current route rather than the caller's earlier lease snapshot.
// A degraded Host can still have live terminal and Gateway sessions.
func (m *Manager) Retry(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.hosts[id]; w != nil && w.session != nil {
		select {
		case w.pollNow <- struct{}{}:
		default:
		}
		return
	}
	m.restartLocked(id)
}

func (m *Manager) restartLocked(id string) {
	if m.ctx.Err() != nil {
		return
	}
	old := m.hosts[id]
	if old != nil {
		old.stop()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w := &worker{cancel: cancel, done: make(chan struct{}), pollNow: make(chan struct{}, 1)}
	m.hosts[id] = w
	m.jobs.Go(func() {
		defer close(w.done)
		defer cancel()
		if old != nil {
			<-old.done
		}
		m.run(ctx, id, w)
	})
}

func (m *Manager) session(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.hosts[id]; w != nil {
		return w.session
	}
	return nil
}

func (m *Manager) Engine(id string) runtime.Engine {
	if s := m.session(id); s != nil {
		return s.docker
	}
	// An SSH Host cannot enroll an Agent. The hub's binding for that exact
	// ID therefore returns ErrHostUnavailable while its SSH route is down.
	return m.agents.Engine(id)
}

func (m *Manager) DialSSH(ctx context.Context, host, id string, address netip.AddrPort) (net.Conn, error) {
	if s := m.session(host); s != nil {
		return s.docker.DialWorkspaceSSH(ctx, id, address, m.network)
	}
	return m.agents.DialSSH(ctx, host, id, address)
}

var (
	errReach      = errors.New("Cannot reach the SSH server. Check its address, port, and network connectivity.")
	errAuth       = errors.New("SSH login failed. Check the username, authentication method, and credentials.")
	errDocker     = errors.New("Cannot access Docker over SSH. Check the remote Docker CLI, daemon, and socket permissions.")
	errNetwork    = errors.New("Cannot prepare the Workspace bridge. Check its name, driver, and Docker permissions.")
	errInventory  = errors.New("Cannot inspect the configured Workspace images on the Docker Host.")
	errConnection = errors.New("The SSH connection closed. Reconnecting.")
	errStorage    = errors.New("Cannot read or update the stored Host connection.")
)

func (m *Manager) connect(ctx context.Context, id string, cfg hostssh.Config) (*session, error) {
	client, err := hostssh.Connect(ctx, cfg)
	if err != nil {
		var ne net.Error
		switch {
		case errors.Is(err, hostssh.ErrHostKeyChanged):
			return nil, hostssh.ErrHostKeyChanged
		case errors.As(err, &ne), errors.Is(err, context.Canceled):
			return nil, errReach
		default:
			return nil, errAuth
		}
	}
	docker, err := runtime.NewRemoteDocker(m.instance, id, client.DialDocker, client.DialContext)
	if err != nil {
		client.Close()
		return nil, errDocker
	}
	return &session{client: client, docker: docker}, nil
}

func (m *Manager) Validate(ctx context.Context, cfg hostssh.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s, err := m.connect(ctx, "", cfg)
	if err != nil {
		return err
	}
	defer s.close()
	_, err = m.checkEngine(ctx, s.docker)
	return err
}

func (m *Manager) checkEngine(ctx context.Context, d *runtime.Docker) (runtime.Host, error) {
	info, err := d.Host(ctx)
	if err != nil {
		return runtime.Host{}, errDocker
	}
	if err = d.EnsureSSHNetwork(ctx, m.network); err != nil {
		return runtime.Host{}, errNetwork
	}
	return info, nil
}

func (m *Manager) run(ctx context.Context, id string, w *worker) {
	for ctx.Err() == nil {
		h, err := m.db.Host(ctx, id)
		if err == nil {
			if h.ConnectionType != "ssh" || h.Scheduling == "disabled" {
				return
			}
			err = m.connected(ctx, h, w)
			if ctx.Err() != nil {
				return
			}
			if e := m.db.SSHFailure(ctx, id, h.SSHRevision, err.Error()); e != nil && !errors.Is(e, store.ErrHostUnauthorized) && ctx.Err() == nil {
				slog.Warn("expire SSH Host lease", "host", id, "error", e)
			}
			if errors.Is(err, hostssh.ErrHostKeyChanged) {
				return
			}
		} else if errors.Is(err, store.ErrMissing) {
			// Registration may have been removed before its first Changed call.
			m.mu.Lock()
			if m.hosts[id] == w {
				delete(m.hosts, id)
			}
			m.mu.Unlock()
			return
		} else if ctx.Err() == nil {
			slog.Warn("read SSH Host", "host", id, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (m *Manager) connected(ctx context.Context, h store.Host, w *worker) error {
	cfg, err := m.db.SSHConfig(ctx, h.ID, m.key)
	if err != nil {
		return errStorage
	}
	s, err := m.connect(ctx, h.ID, cfg)
	if err != nil {
		return err
	}
	tick := time.NewTicker(8 * time.Second)
	defer tick.Stop()
	return m.monitor(ctx, h, w, s, tick.C)
}

func (m *Manager) monitor(ctx context.Context, h store.Host, w *worker, s *session, polls <-chan time.Time) error {
	ctx, cancel := context.WithCancel(ctx)
	var ready sync.WaitGroup
	defer func() {
		cancel()
		m.mu.Lock()
		w.session = nil
		m.mu.Unlock()
		s.close()
		ready.Wait()
	}()
	closed := make(chan struct{})
	go func() { s.client.Wait(); close(closed) }()
	healthy := false
	for {
		report, probeErr := m.inventory(ctx, h.ID, s.docker)
		if probeErr != nil {
			// A failed Docker call can leave the SSH connection and its active
			// streams intact. A bounded protocol round trip distinguishes this
			// from a half-open connection that Wait has not detected yet.
			if !s.responsive(ctx) {
				return errConnection
			}
			if err := m.db.SSHDegraded(ctx, h.ID, h.SSHRevision, probeErr.Error()); err != nil {
				return errStorage
			}
			healthy = false
		} else {
			if !healthy {
				m.mu.Lock()
				if m.hosts[h.ID] != w || ctx.Err() != nil {
					m.mu.Unlock()
					return errConnection
				}
				w.session = s
				m.mu.Unlock()
			}
			// A newly visible online lease must already have a routable engine.
			if err := m.db.SSHHeartbeat(ctx, h.ID, h.SSHRevision, report); err != nil {
				return errStorage
			}
			if !healthy && m.onReady != nil {
				ready.Go(func() { m.onReady(ctx, h.ID, s.docker) })
			}
			healthy = true
		}
		select {
		case <-closed:
			return errConnection
		case <-ctx.Done():
			return errConnection
		case <-polls:
		case <-w.pollNow:
		}
	}
}

func (s *session) responsive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// Even an "unsupported request" SSH reply proves a live round trip.
		_, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return false // The caller closes the transport, unblocking SendRequest.
	case err := <-done:
		return err == nil
	}
}

func (m *Manager) inventory(ctx context.Context, id string, d *runtime.Docker) (store.HostReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	info, err := m.checkEngine(ctx, d)
	if err != nil {
		return store.HostReport{}, err
	}
	images, err := m.db.DesiredWorkspaceImages(ctx)
	if err != nil {
		return store.HostReport{}, errStorage
	}
	statuses := make([]store.ImageStatus, 0, len(images))
	for _, image := range images {
		ready, err := d.HasImage(ctx, image)
		if err != nil {
			return store.HostReport{}, errInventory
		}
		statuses = append(statuses, store.ImageStatus{Image: image, Ready: ready})
	}
	limit, err := m.db.HostMaxOperationsTarget(ctx, id)
	if err != nil {
		return store.HostReport{}, errStorage
	}
	return store.HostReport{
		Engine: info.Engine, OS: info.OS, Arch: info.Arch, Healthy: true, Images: statuses,
		CPU: int64(info.CPUs) * 1000, Memory: info.Memory, MaxOperations: limit,
		RunningContainers: info.Running, TotalContainers: info.Total,
	}, nil
}
