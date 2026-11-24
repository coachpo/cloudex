package main

import (
	"context"
	"crypto/cipher"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentauth"
	"github.com/coachpo/cloudex/apps/api/internal/agentlink"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
)

type agentServices struct {
	*agentlink.Hub
	caPEM  string
	exited <-chan error
	close  func()
}

func startAgentServices(ctx context.Context, db *store.Store, instance string, key cipher.AEAD, onReady func(context.Context, string, runtime.Engine)) (agentServices, error) {
	if err := db.ExpireHostLease(ctx, ""); err != nil {
		return agentServices{}, err
	}
	a, err := agentauth.Open(ctx, db, instance, key)
	if err != nil {
		return agentServices{}, err
	}
	authenticate := func(ctx context.Context) (string, string, error) {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return "", "", errors.New("missing Agent TLS identity")
		}
		tls, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok {
			return "", "", errors.New("missing Agent TLS identity")
		}
		return a.HostIdentity(ctx, &tls.State)
	}
	configuration := agentConfigSync{db: db}
	hub := agentlink.NewHub(
		func(ctx context.Context) (string, error) { host, _, err := authenticate(ctx); return host, err },
		func(ctx context.Context, host string, inventory agentlink.Inventory, configure agentlink.ConfigureAgent) error {
			registered, key, err := authenticate(ctx)
			if err != nil || host != registered {
				return store.ErrHostUnauthorized
			}
			h := inventory.Host
			if err := db.Heartbeat(ctx, host, key, store.HostReport{
				AgentVersion: inventory.Version, Engine: h.Engine, OS: h.OS, Arch: h.Arch, Images: reportedImages(inventory),
				Healthy: inventory.Error == "",
				CPU:     int64(h.CPUs) * 1000, Memory: h.Memory,
				MaxOperations: inventory.MaxConcurrent, RunningContainers: h.Running, TotalContainers: h.Total,
			}); err != nil {
				return err
			}
			configuration.reconcile(ctx, host, inventory, configure)
			return nil
		},
		func(ctx context.Context, host string) {
			if err := db.ExpireHostLease(ctx, host); err != nil {
				slog.Warn("expire disconnected Host lease", "host", host, "error", err)
			}
		},
		onReady,
	)
	// Agents ping every 20 s (cmd/host-agent); allow that without GOAWAY.
	rpc := grpc.NewServer(grpc.Creds(negotiatedTLS{}),
		// Stop cancels the streams and waits for disconnect handlers before SQLite closes.
		grpc.WaitForHandlers(true),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}))
	hub.Register(rpc)
	tlsConfig, err := a.TLSConfig()
	if err != nil {
		return agentServices{}, err
	}
	listener, err := net.Listen("tcp", env("CLOUDEX_AGENT_LISTEN", "127.0.0.1:9443"))
	if err != nil {
		return agentServices{}, fmt.Errorf("listen for Host Agents: %w", err)
	}
	socket, err := listenBootstrap(env("CLOUDEX_AGENT_BOOTSTRAP_SOCKET", "data/agent-enroll.sock"))
	if err != nil {
		listener.Close()
		return agentServices{}, fmt.Errorf("listen for local Host Agent enrollment: %w", err)
	}
	// HTTP/2 connections go to gRPC's own transport, which enforces per-stream
	// flow control. grpc.Server.ServeHTTP has none, so a terminal or exec
	// stream the consumer reads slowly would be buffered here without bound.
	streams := newConnListener(listener.Addr())
	enrollment := a.Enrollment(false, hub.Disconnect)
	agents := &http.Server{
		TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){"h2": streams.serve},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
			enrollment.ServeHTTP(w, r)
		}),
	}
	bootstrap := &http.Server{
		Handler: a.Enrollment(true, hub.Disconnect), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	exited := make(chan error, 3)
	go func() {
		slog.Info("Host Agent listener ready", "address", listener.Addr())
		exited <- agents.ServeTLS(listener, "", "")
	}()
	go func() { exited <- bootstrap.Serve(socket) }()
	go func() {
		if err := rpc.Serve(streams); err != nil {
			exited <- err
		}
	}()
	return agentServices{Hub: hub, caPEM: a.CertificatePEM(), exited: exited, close: func() {
		_ = agents.Close()
		_ = bootstrap.Close()
		rpc.Stop()
		_ = streams.Close()
	}}, nil
}

func reportedImages(inventory agentlink.Inventory) []store.ImageStatus {
	images := make([]store.ImageStatus, 0, len(inventory.Images))
	for _, status := range inventory.Images {
		images = append(images, store.ImageStatus{Image: status.Image, Ready: status.Ready})
	}
	return images
}

func sameImageSet(reported []agentlink.ImageStatus, desired []string) bool {
	seen := make(map[string]struct{}, len(reported))
	for _, status := range reported {
		seen[status.Image] = struct{}{}
	}
	if len(seen) != len(desired) {
		return false
	}
	for _, image := range desired {
		if _, ok := seen[image]; !ok {
			return false
		}
	}
	return true
}

type agentConfigSync struct {
	db       *store.Store
	inFlight sync.Map
}

func (s *agentConfigSync) reconcile(ctx context.Context, host string, inventory agentlink.Inventory, configure agentlink.ConfigureAgent) {
	if _, busy := s.inFlight.LoadOrStore(host, struct{}{}); busy {
		return
	}
	// The control stream must keep receiving frames while the configuration call runs.
	go func() {
		defer s.inFlight.Delete(host)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		target, err := s.db.HostMaxOperationsTarget(ctx, host)
		if err != nil {
			slog.Warn("read Host Agent operation limit", "host", host, "error", err)
			return
		}
		images, err := s.db.DesiredWorkspaceImages(ctx)
		if err != nil {
			slog.Warn("read Host Agent workspace images", "host", host, "error", err)
			return
		}
		if target == inventory.MaxConcurrent && sameImageSet(inventory.Images, images) {
			return
		}
		if err := configure(ctx, agentlink.AgentConfig{MaxConcurrent: target, Images: images}); err != nil {
			slog.Warn("configure Host Agent", "host", host, "error", err)
		}
	}()
}

// connListener hands TLS connections that negotiated h2 on the shared Agent
// port to grpc.Server.Serve. net/http closes such a connection when its
// TLSNextProto handler returns, so serve waits until gRPC is done with it.
type connListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{addr: addr, conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *connListener) Addr() net.Addr { return l.addr }

func (l *connListener) serve(_ *http.Server, conn *tls.Conn, _ http.Handler) {
	c := &handedConn{Conn: conn, done: make(chan struct{})}
	select {
	case l.conns <- c:
	case <-l.closed:
		return
	}
	select {
	case <-c.done:
	case <-l.closed:
	}
}

type handedConn struct {
	*tls.Conn
	once sync.Once
	done chan struct{}
}

func (c *handedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// negotiatedTLS exposes the handshake net/http already completed, including
// the verified Agent client certificate, as gRPC peer authentication.
type negotiatedTLS struct{}

func (negotiatedTLS) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	tlsConn, ok := conn.(interface{ ConnectionState() tls.ConnectionState })
	if !ok {
		return nil, nil, errors.New("Host Agent connection is not TLS")
	}
	return conn, credentials.TLSInfo{State: tlsConn.ConnectionState(),
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}}, nil
}

func (negotiatedTLS) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("server-side credentials only")
}

func (negotiatedTLS) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.3"}
}

func (c negotiatedTLS) Clone() credentials.TransportCredentials { return c }

func (negotiatedTLS) OverrideServerName(string) error { return nil }

func listenBootstrap(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("bootstrap path %q is occupied", path)
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("bootstrap socket %q is already listening", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
