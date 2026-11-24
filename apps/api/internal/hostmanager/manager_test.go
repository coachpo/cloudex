package hostmanager

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentlink"
	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/ssh"
)

func TestRestartLeavesChangedKeyOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	signer := func() ssh.Signer {
		t.Helper()
		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ssh.NewSignerFromKey(privateKey)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
		t.Error("attempted authentication with changed host identity")
		return nil, errors.New("refused")
	}}
	server.AddHostKey(signer())
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			ssh.NewServerConn(conn, server)
		}
	}()
	cfg := hostssh.Config{Address: listener.Addr().String(), User: "test", Password: "test", HostKey: string(ssh.MarshalAuthorizedKey(signer().PublicKey()))}
	h, err := db.AddSSHHost(ctx, "remote", cfg, key)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(ctx, db, "test-instance", "workspaces", key, agentlink.NewHub(nil, nil, nil, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	select {
	case <-manager.hosts[h.ID].done:
	case <-ctx.Done():
		t.Fatal("manager did not pause reconnects after SSH host identity changed")
	}
	<-accepted
	h, err = db.Host(ctx, h.ID)
	if err != nil || h.Status != "offline" || h.ConnectionError != hostssh.ErrHostKeyChanged.Error() {
		t.Fatalf("unexpected Host state status=%q connection_error=%q err=%v", h.Status, h.ConnectionError, err)
	}
	if _, err = manager.Engine(h.ID).Host(ctx); !errors.Is(err, runtime.ErrHostUnavailable) {
		t.Fatalf("unexpected runtime binding for paused Host: %v", err)
	}
}

// monitoringSSH represents the SSH control connection while Docker requests
// use the HTTP test daemon. Closing it also closes any active forwarded streams.
type monitoringSSH struct {
	ssh.Conn
	closed chan struct{}
}

func (c *monitoringSSH) Close() error { close(c.closed); return nil }
func (c *monitoringSSH) Wait() error  { <-c.closed; return io.EOF }
func (c *monitoringSSH) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return false, nil, nil // a live server can reject the optional keepalive request
}

func TestInventoryFailurePreservesSessionsAndResyncsOnRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "hosts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	block, _ := aes.NewCipher(make([]byte, 32))
	key, _ := cipher.NewGCM(block)
	h, err := db.AddSSHHost(ctx, "test", hostssh.Config{}, key)
	if err != nil {
		t.Fatal(err)
	}

	var failing atomic.Bool
	failedInspections := make(chan struct{}, 2)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/_ping":
			w.Header().Set("Api-Version", "1.54")
		case strings.HasSuffix(r.URL.Path, "/info"):
			io.WriteString(w, `{"Name":"test","ServerVersion":"29","NCPU":2,"MemTotal":2000}`)
		case strings.HasSuffix(r.URL.Path, "/networks/workspaces"):
			io.WriteString(w, `{"Name":"workspaces","Driver":"bridge","Scope":"local","IPAM":{"Config":[{"Subnet":"172.20.0.0/16"}]}}`)
		case strings.Contains(r.URL.Path, "/images/"):
			if failing.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"message":"temporary Docker error"}`)
				failedInspections <- struct{}{}
			} else {
				io.WriteString(w, `{}`)
			}
		default:
			t.Errorf("unexpected Docker request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer daemon.Close()
	d, err := runtime.NewRemoteDocker("test-instance", h.ID, func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", daemon.Listener.Addr().String())
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := &monitoringSSH{closed: make(chan struct{})}
	s := &session{docker: d, client: &hostssh.Client{Client: &ssh.Client{Conn: conn}}}
	w := &worker{pollNow: make(chan struct{}, 1)}
	ready := make(chan struct{}, 2)
	m := &Manager{db: db, network: "workspaces", hosts: map[string]*worker{h.ID: w}, onReady: func(context.Context, string, runtime.Engine) { ready <- struct{}{} }}
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	go func() { defer close(stopped); m.monitor(ctx, h, w, s, ticks) }()
	defer func() { cancel(); <-stopped }()
	awaitReady := func() {
		t.Helper()
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-stopped:
			t.Fatal("SSH monitor closed active connection")
		}
	}
	poll := func() {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-stopped:
			t.Fatal("SSH monitor closed active connection")
		}
	}
	awaitReady()
	failing.Store(true)
	poll()
	// Synchronize with completion of the failed poll before checking the lease.
	poll()
	// Both scheduled polls must observe the fault. Only the manual retry below
	// can then observe the recovery.
	for range 2 {
		select {
		case <-failedInspections:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	status, err := db.Host(ctx, h.ID)
	if err != nil || status.Status != "degraded" || status.ConnectionError == "" || status.Capacity.CPU != 2000 || len(status.Images) != 1 {
		t.Fatalf("failed observation lost last inventory or kept Host eligible: %+v, %v", status, err)
	}
	select {
	case <-conn.closed:
		t.Fatal("inventory failure closed existing SSH streams")
	default:
	}
	failing.Store(false)
	m.Retry(h.ID)
	awaitReady()
	status, err = db.Host(ctx, h.ID)
	if err != nil || status.Status != "online" || status.ConnectionError != "" || m.session(h.ID) != s {
		t.Fatalf("Host did not recover on original connection: %+v, %v", status, err)
	}
}
