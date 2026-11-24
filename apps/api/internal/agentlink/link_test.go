package agentlink

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type fakeRuntime struct {
	runtime.Engine
	container runtime.Container
	run       func(context.Context, runtime.Exec) (int, error)
	purge     func(context.Context) error
	start     func(context.Context) error
	terminal  runtime.Terminal
}

func (f *fakeRuntime) Inspect(context.Context, runtime.Spec) (runtime.Container, error) {
	if f.container.ID == "" {
		return runtime.Container{}, runtime.ErrMissing
	}
	return f.container, nil
}

func (f *fakeRuntime) Run(ctx context.Context, _ string, exec runtime.Exec) (int, error) {
	return f.run(ctx, exec)
}

func (f *fakeRuntime) Start(ctx context.Context, _ string) error { return f.start(ctx) }

func (f *fakeRuntime) Purge(ctx context.Context, _ string, _ bool) error { return f.purge(ctx) }
func (f *fakeRuntime) Terminal(context.Context, string) (runtime.Terminal, error) {
	return f.terminal, nil
}

func testHub(t *testing.T, disconnected func(context.Context, string)) (*Hub, *httptest.Server, <-chan string) {
	t.Helper()
	heartbeats := make(chan string, 32)
	hub, server := testHubWithHeartbeat(t, func(ctx context.Context, id string, inventory Inventory, _ ConfigureAgent) error {
		if id != inventory.Host.Name || inventory.MaxConcurrent != 2 {
			return fmt.Errorf("inventory mismatch: %s", id)
		}
		select {
		case heartbeats <- id:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}, disconnected)
	return hub, server, heartbeats
}

func testHubWithHeartbeat(t *testing.T, heartbeat func(context.Context, string, Inventory, ConfigureAgent) error, disconnected func(context.Context, string)) (*Hub, *httptest.Server) {
	t.Helper()
	hub := NewHub(func(ctx context.Context) (string, error) {
		p, ok := grpcpeer.FromContext(ctx)
		if !ok {
			return "", errors.New("missing peer")
		}
		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(tlsInfo.State.PeerCertificates) != 1 {
			return "", errors.New("missing TLS identity")
		}
		return tlsInfo.State.PeerCertificates[0].Subject.CommonName, nil
	}, heartbeat, disconnected)
	rpc := grpc.NewServer()
	hub.Register(rpc)
	server := httptest.NewUnstartedServer(rpc)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(func() { rpc.Stop(); server.Close() })
	return hub, server
}

func testAgentClient(t *testing.T, server *httptest.Server, host string) *grpc.ClientConn {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tlsConfig := &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}, MinVersion: tls.VersionTLS13}
	client, err := grpc.NewClient(server.Listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func startTestAgent(t *testing.T, server *httptest.Server, host string, agent Agent) func() {
	t.Helper()
	client := testAgentClient(t, server, host)
	agent.MaxConcurrent = 2
	agent.Inventory = func(context.Context) (Inventory, error) {
		return Inventory{Host: runtime.Host{Name: host, CPUs: 4}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := agent.Run(ctx, client); err != nil {
			t.Error(err)
		}
	}()
	stop := func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("agent failed to stop")
		}
		client.Close()
	}
	t.Cleanup(stop)
	return stop
}

func TestConfigureAgentReportsAppliedLimitAndRejectsStaleSession(t *testing.T) {
	type report struct {
		inventory Inventory
		configure ConfigureAgent
	}
	beats := make(chan report, 4)
	_, server := testHubWithHeartbeat(t, func(ctx context.Context, _ string, inventory Inventory, configure ConfigureAgent) error {
		select {
		case beats <- report{inventory, configure}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	const host = "configurable"
	client := testAgentClient(t, server, host)
	defer client.Close()
	a := executor{
		Agent: Agent{
			Inventory: func(context.Context) (Inventory, error) {
				return Inventory{Host: runtime.Host{Name: host}}, nil
			},
			HasImage: func(context.Context, string) (bool, error) { return true, nil },
		},
		mutations: mutationSemaphore{limit: 3},
		images:    []string{"first"},
	}
	next := func(image string) report {
		t.Helper()
		for {
			select {
			case beat := <-beats:
				if len(beat.inventory.Images) == 1 && beat.inventory.Images[0].Image == image {
					return beat
				}
			case <-ctx.Done():
				t.Fatal("did not receive heartbeat")
			}
		}
	}
	connect := func() func() {
		link, stop := context.WithCancel(ctx)
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			_ = a.connect(link, client)
		}()
		return func() {
			stop()
			<-closed
			a.work.Wait()
		}
	}
	stopFirst := connect()
	defer stopFirst()
	first := next("first")
	if got := first.inventory.MaxConcurrent; got != 3 {
		t.Fatalf("initial heartbeat limit = %d, want 3", got)
	}
	if err := first.configure(ctx, AgentConfig{MaxConcurrent: 0}); err == nil {
		t.Fatal("agent accepted a limit below 1")
	}
	if err := first.configure(ctx, AgentConfig{MaxConcurrent: 5, Images: []string{"second"}}); err != nil {
		t.Fatalf("configure agent: %v", err)
	}
	stopFirst()
	stopSecond := connect()
	defer stopSecond()
	second := next("second")
	if got := second.inventory.MaxConcurrent; got != 5 {
		t.Fatalf("heartbeat after reconnect = %d, want configured limit 5", got)
	}
	if err := first.configure(ctx, AgentConfig{MaxConcurrent: 1}); !errors.Is(err, runtime.ErrHostUnavailable) {
		t.Fatalf("stale configure error = %v, want host unavailable", err)
	}
}

func TestDelayedRevokedConnectionDoesNotReplaceAuthorizedAgent(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	host := "rotating"
	oldAuthorized := make(chan struct{})
	resumeOld := make(chan struct{})
	var revoked atomic.Bool
	authenticate := hub.authenticate
	hub.authenticate = func(ctx context.Context) (string, error) {
		id, err := authenticate(ctx)
		if len(metadata.ValueFromIncomingContext(ctx, "old-connection")) == 0 {
			return id, err
		}
		if revoked.Load() {
			return "", errors.New("old certificate revoked")
		}
		close(oldAuthorized)
		select {
		case <-resumeOld:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return id, err
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	oldClient := testAgentClient(t, server, host)
	defer oldClient.Close()
	old, err := openStream(metadata.AppendToOutgoingContext(ctx, "old-connection", "true"), oldClient, controlStream)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, err := valueFrame("heartbeat", Inventory{Host: runtime.Host{Name: host}, MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := send(old, heartbeat); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldAuthorized:
	case <-ctx.Done():
		t.Fatal("old connection did not reach authentication")
	}
	revoked.Store(true)
	hub.Disconnect(host)
	tty, attached := net.Pipe()
	defer attached.Close()
	startTestAgent(t, server, host, Agent{Engine: &fakeRuntime{terminal: fakeTerminal{Conn: tty}}})
	awaitHeartbeat(t, beats, host)
	terminal, err := hub.Engine(host).Terminal(ctx, "container")
	if err != nil {
		t.Fatalf("replacement did not open a data stream: %v", err)
	}
	defer terminal.Close()

	close(resumeOld)
	if _, err := receive(old); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked connection was not rejected: %v", err)
	}
	if _, err := io.WriteString(terminal, "x"); err != nil {
		t.Fatalf("delayed connection closed the replacement's data stream: %v", err)
	}
	_ = attached.SetReadDeadline(time.Now().Add(2 * time.Second))
	var input [1]byte
	if _, err := io.ReadFull(attached, input[:]); err != nil || input[0] != 'x' {
		t.Fatalf("replacement did not receive data on its original stream: %q, %v", input, err)
	}
}

func awaitHeartbeat(t *testing.T, beats <-chan string, host string) {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case id := <-beats:
			if id == host {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for heartbeat from %s", host)
		}
	}
}

func TestRuntimeStreamsUseTheAssignedHost(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	target := netip.MustParseAddr("172.20.0.6")
	remote := &fakeRuntime{
		container: runtime.Container{ID: "remote-container", SSHRevision: "revision", NetworkIPs: map[string]netip.Addr{"private": target}},
		start:     func(context.Context) error { return runtime.ErrStartOutcomeUnknown },
		run: func(_ context.Context, exec runtime.Exec) (int, error) {
			if !slices.Equal(exec.Args, []string{"workspace-control", "git"}) || exec.Dir != runtime.WorkDir {
				return -1, errors.New("unexpected Git exec specification")
			}
			_, _ = io.WriteString(exec.Stdout, "stdout")
			_, _ = io.WriteString(exec.Stderr, "stderr")
			return 42, nil
		},
	}
	startTestAgent(t, server, "local", Agent{Engine: &fakeRuntime{container: runtime.Container{ID: "local-container"}}})
	awaitHeartbeat(t, beats, "local")
	startTestAgent(t, server, "remote", Agent{Engine: remote, ValidateContainer: func(_ context.Context, id string) error {
		if id != remote.container.ID {
			return runtime.ErrOwnership
		}
		return nil
	}})
	awaitHeartbeat(t, beats, "remote")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := hub.Engine("remote")
	c, err := engine.Inspect(ctx, runtime.Spec{WorkspaceID: "one", Generation: 1})
	if err != nil || c.ID != "remote-container" || c.SSHRevision != "revision" || c.NetworkIPs["private"] != target {
		t.Fatalf("inspect returned %#v, %v", c, err)
	}
	// Docker answered; the control plane verifies the start instead of
	// replaying it as if the Host were lost.
	if err := engine.Start(ctx, c.ID); !errors.Is(err, runtime.ErrStartOutcomeUnknown) || errors.Is(err, runtime.ErrHostUnavailable) {
		t.Fatalf("a Docker start reply must keep its outcome and not report a lost Host: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code, err := engine.Run(ctx, c.ID, runtime.Exec{
		Args: []string{"workspace-control", "git"}, Dir: runtime.WorkDir, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil || code != 42 || stdout.String() != "stdout" || stderr.String() != "stderr" {
		t.Fatalf("run returned %d %q %q, err=%v", code, &stdout, &stderr, err)
	}
	_, err = engine.Run(ctx, "local-container", runtime.Exec{})
	if !errors.Is(err, runtime.ErrOwnership) {
		t.Fatalf("cross-host container use returned %v", err)
	}
	hub.Disconnect("remote")
	if _, err = engine.Inspect(ctx, runtime.Spec{}); !errors.Is(err, runtime.ErrHostUnavailable) {
		t.Fatalf("disconnected host returned %v", err)
	}
	local, err := hub.Engine("local").Inspect(ctx, runtime.Spec{})
	if err != nil || local.ID != "local-container" {
		t.Fatalf("disconnect affected another host: %#v, %v", local, err)
	}
}

func TestPurgeResultIsRecoveredAfterDisconnectAndAgentRestart(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	var invocations atomic.Int32
	entered, settle := make(chan struct{}), make(chan struct{})
	agent := Agent{JournalPath: filepath.Join(t.TempDir(), "journal.sqlite"), Engine: &fakeRuntime{
		purge: func(ctx context.Context) error {
			if invocations.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-settle:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}}
	stop := startTestAgent(t, server, "purger", agent)
	awaitHeartbeat(t, beats, "purger")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ctx = WithOperation(ctx, "op", "ws")
	engine := hub.Engine("purger")
	result := make(chan error, 1)
	go func() { result <- engine.Purge(ctx, "ws", false) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("purge never began")
	}
	hub.Disconnect("purger")
	if err := <-result; !errors.Is(err, runtime.ErrHostUnavailable) {
		t.Fatalf("purge did not report an unknown outcome: %v", err)
	}
	close(settle)
	awaitHeartbeat(t, beats, "purger")
	if err := engine.Purge(ctx, "ws", false); err != nil {
		t.Fatalf("replay after reconnect: %v", err)
	}
	stop()
	startTestAgent(t, server, "purger", agent)
	awaitHeartbeat(t, beats, "purger")
	if err := engine.Purge(ctx, "ws", false); err != nil || invocations.Load() != 1 {
		t.Fatalf("replay re-executed a completed purge: invocations=%d err=%v", invocations.Load(), err)
	}
}

func TestSSHTunnelPreservesHalfClose(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		socket, err := listener.Accept()
		if err != nil {
			return
		}
		defer socket.Close()
		if input, err := io.ReadAll(socket); err == nil {
			_, _ = fmt.Fprintf(socket, "ack: %s", input)
		}
	}()
	address := listener.Addr().(*net.TCPAddr).AddrPort()
	startTestAgent(t, server, "ssh-host", Agent{
		Engine: &fakeRuntime{},
		DialSSH: func(ctx context.Context, id string, target netip.AddrPort) (net.Conn, error) {
			if id != "c" || target != address {
				return nil, runtime.ErrOwnership
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.String())
		},
	})
	awaitHeartbeat(t, beats, "ssh-host")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := hub.DialSSH(ctx, "ssh-host", "c", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = io.WriteString(conn, "workspace"); err != nil {
		t.Fatal(err)
	}
	if err = conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil || string(data) != "ack: workspace" {
		t.Fatalf("relay response %q, %v", data, err)
	}
}

type fakeTerminal struct {
	net.Conn
	dimensions chan dimensions
}

func (t fakeTerminal) Resize(ctx context.Context, cols, rows uint) error {
	select {
	case t.dimensions <- dimensions{Cols: cols, Rows: rows}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestTerminalStreamsInputAndResizeUntilHostDisconnects(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	tty, attached := net.Pipe()
	defer attached.Close()
	resized := make(chan dimensions, 1)
	startTestAgent(t, server, "terminal-host", Agent{Engine: &fakeRuntime{
		terminal: fakeTerminal{Conn: tty, dimensions: resized},
	}})
	awaitHeartbeat(t, beats, "terminal-host")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	terminal, err := hub.Engine("terminal-host").Terminal(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := terminal.Resize(ctx, 108, 40); err != nil {
		t.Fatal(err)
	}
	select {
	case dims := <-resized:
		if dims != (dimensions{Cols: 108, Rows: 40}) {
			t.Fatalf("unexpected dimensions %#v", dims)
		}
	case <-ctx.Done():
		t.Fatal("terminal was not resized")
	}
	_ = attached.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(terminal, "input"); err != nil {
		t.Fatal(err)
	}
	var input [5]byte
	if _, err := io.ReadFull(attached, input[:]); err != nil || string(input[:]) != "input" {
		t.Fatalf("agent received %q, %v", input, err)
	}
	if _, err := io.WriteString(attached, "output"); err != nil {
		t.Fatal(err)
	}
	var output [6]byte
	if _, err := io.ReadFull(terminal, output[:]); err != nil || string(output[:]) != "output" {
		t.Fatalf("control plane received %q, %v", output, err)
	}
	go hub.Disconnect("terminal-host")
	if n, err := terminal.Read(output[:]); n != 0 || err == nil || ctx.Err() != nil {
		t.Fatalf("disconnect did not promptly close terminal: bytes=%d err=%v ctx=%v", n, err, ctx.Err())
	}
}

func TestClosingAgentConnectionReportsHostOffline(t *testing.T) {
	offline := make(chan string, 1)
	_, server, beats := testHub(t, func(ctx context.Context, hostID string) {
		if ctx.Err() != nil {
			t.Errorf("disconnect callback had no usable context: %v", ctx.Err())
		}
		offline <- hostID
	})
	stop := startTestAgent(t, server, "disconnecting", Agent{Engine: &fakeRuntime{}})
	awaitHeartbeat(t, beats, "disconnecting")
	stop()
	select {
	case hostID := <-offline:
		if hostID != "disconnecting" {
			t.Fatalf("unexpected offline host %q", hostID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the connection did not report the Host offline")
	}
}

func TestReconnectWaitsForPreviousLeaseExpiration(t *testing.T) {
	expiring := make(chan struct{})
	expire := make(chan struct{})
	var disconnects atomic.Int32
	_, server, beats := testHub(t, func(ctx context.Context, hostID string) {
		if hostID == "reconnecting" && disconnects.Add(1) == 1 {
			close(expiring)
			select {
			case <-ctx.Done():
			case <-expire:
			}
		}
	})
	defer close(expire)
	stop := startTestAgent(t, server, "reconnecting", Agent{Engine: &fakeRuntime{}})
	awaitHeartbeat(t, beats, "reconnecting")
	stop()
	select {
	case <-expiring:
	case <-time.After(5 * time.Second):
		t.Fatal("previous lease did not begin expiring")
	}

	startTestAgent(t, server, "reconnecting", Agent{Engine: &fakeRuntime{}})
	startTestAgent(t, server, "independent", Agent{Engine: &fakeRuntime{}})
	deadline := time.After(time.Second)
	independent := false
waiting:
	for {
		select {
		case host := <-beats:
			if host == "reconnecting" {
				t.Fatal("replacement heartbeat would be overwritten by expiration of the previous lease")
			}
			independent = true
		case <-deadline:
			break waiting
		}
	}
	if !independent {
		t.Fatal("expiring one Host lease blocked another Host")
	}
	expire <- struct{}{}
	awaitHeartbeat(t, beats, "reconnecting")
}
