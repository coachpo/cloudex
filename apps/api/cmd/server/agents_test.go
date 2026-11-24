package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentauth"
	"github.com/coachpo/cloudex/apps/api/internal/agentlink"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type inventoryOnlyEngine struct{ runtime.Engine }

func TestAgentConfigReconcilesWithoutBlockingHeartbeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		db, err := store.Open(ctx, filepath.Join(t.TempDir(), "cloud.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		sync := agentConfigSync{db: db}
		const host = store.LocalHostID
		setLimit := func(target int) {
			t.Helper()
			if _, err := db.SetHostMaxOperations(ctx, host, target); err != nil {
				t.Fatal(err)
			}
		}
		calls := make(chan agentlink.AgentConfig, 3)
		finish := make(chan error)
		configure := func(ctx context.Context, config agentlink.AgentConfig) error {
			calls <- config
			select {
			case err := <-finish:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		noDispatch := func() {
			t.Helper()
			synctest.Wait()
			select {
			case config := <-calls:
				t.Fatalf("unexpected configuration: %+v", config)
			default:
			}
		}
		heartbeat := agentlink.Inventory{
			Version: "2.5.0", MaxConcurrent: 3,
			Images: []agentlink.ImageStatus{{Image: store.DefaultWorkspaceImage, Ready: true}},
		}
		setLimit(3)
		sync.reconcile(ctx, host, heartbeat, configure)
		noDispatch()
		setLimit(4)
		sync.reconcile(ctx, host, heartbeat, configure)
		synctest.Wait()
		select {
		case config := <-calls:
			if config.MaxConcurrent != 4 || !slices.Equal(config.Images, []string{store.DefaultWorkspaceImage}) {
				t.Fatalf("dispatched %+v, want limit 4 with the default image", config)
			}
		default:
			t.Fatal("configuration did not start")
		}
		// The first call is still waiting for its reply. Heartbeats can continue
		// without starting a second configuration for this Host.
		setLimit(5)
		sync.reconcile(ctx, host, heartbeat, configure)
		noDispatch()
		finish <- errors.New("test Agent rejected the update")
		synctest.Wait()
		sync.reconcile(ctx, host, heartbeat, configure)
		synctest.Wait()
		select {
		case config := <-calls:
			if config.MaxConcurrent != 5 {
				t.Fatalf("dispatched %+v after failed attempt, want limit 5", config)
			}
		default:
			t.Fatal("next heartbeat did not retry")
		}
		finish <- nil
		synctest.Wait()
		heartbeat.MaxConcurrent = 5
		for range 2 {
			sync.reconcile(ctx, host, heartbeat, configure)
			noDispatch()
		}
	})
}

func TestAgentImageSetReconcilesIndependentlyOfLimitAndReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		db, err := store.Open(ctx, filepath.Join(t.TempDir(), "cloud.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		const host = store.LocalHostID
		if _, err := db.Create(ctx, store.Workspace{
			ID: store.ID(), Name: "old", Image: "workspace:old", Branch: "main",
			Desired: "stopped", CPU: 100, Memory: 256,
		}, "", "", nil); err != nil {
			t.Fatal(err)
		}
		sync := agentConfigSync{db: db}
		calls := make(chan agentlink.AgentConfig, 1)
		configure := func(_ context.Context, config agentlink.AgentConfig) error {
			calls <- config
			return nil
		}
		heartbeat := agentlink.Inventory{Version: "2.5.0", MaxConcurrent: 3}
		sync.reconcile(ctx, host, heartbeat, configure)
		synctest.Wait()
		select {
		case config := <-calls:
			if config.MaxConcurrent != 3 || !slices.Equal(config.Images, []string{store.DefaultWorkspaceImage, "workspace:old"}) {
				t.Fatalf("configuration for image-only mismatch: %+v", config)
			}
		default:
			t.Fatal("an image-only mismatch did not dispatch configuration")
		}
		// Reports can arrive in another order, including images absent in Docker.
		heartbeat.Images = []agentlink.ImageStatus{{Image: "workspace:old"}, {Image: store.DefaultWorkspaceImage, Ready: true}}
		sync.reconcile(ctx, host, heartbeat, configure)
		synctest.Wait()
		select {
		case config := <-calls:
			t.Fatalf("matching set dispatched configuration: %+v", config)
		default:
		}
	})
}

type agentServicesFixture struct {
	db    *store.Store
	path  string
	conn  *grpc.ClientConn
	close func()
}

func startTestAgentServices(t *testing.T, ctx context.Context) agentServicesFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cloud.sqlite")
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	t.Setenv("CLOUDEX_AGENT_LISTEN", address)
	t.Setenv("CLOUDEX_AGENT_BOOTSTRAP_SOCKET", socket)

	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	instance, err := db.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	services, err := startAgentServices(ctx, db, instance, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	closeServices := sync.OnceFunc(services.close)
	t.Cleanup(closeServices)

	client := agentauth.Client{Address: address, Socket: socket, StateDir: dir}
	identity, err := client.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tls, err := identity.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tls)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentServicesFixture{db: db, path: path, conn: conn, close: closeServices}
}

func TestAgentShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture := startTestAgentServices(t, ctx)
	db := fixture.db
	agent := agentlink.Agent{
		Engine:   inventoryOnlyEngine{},
		HasImage: func(context.Context, string) (bool, error) { return true, nil },
		Inventory: func(context.Context) (agentlink.Inventory, error) {
			return agentlink.Inventory{Version: "2.5.0"}, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx, fixture.conn) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ready := time.After(5 * time.Second)
	for {
		host, err := db.Host(ctx, store.LocalHostID)
		if err != nil {
			t.Fatal(err)
		}
		if host.Status == "online" {
			break
		}
		select {
		case <-ready:
			t.Fatal("agent did not publish a lease")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Block the disconnect lease update while the active gRPC stream shuts down.
	locker, err := sql.Open("sqlite", fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	tx, err := locker.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), "UPDATE host_nodes SET healthy=healthy WHERE id=?", store.LocalHostID); err != nil {
		t.Fatal(err)
	}
	cancel()
	closed := make(chan struct{})
	go func() {
		fixture.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Agent services closed while the disconnect lease update was blocked")
	case <-time.After(250 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Agent services did not close")
	}
	host, err := db.Host(t.Context(), store.LocalHostID)
	if err != nil || host.Status != "offline" {
		t.Fatalf("lease did not expire before services closed: %s, %v", host.Status, err)
	}
}

func TestAgentHeartbeatConfigure(t *testing.T) {
	fixture := startTestAgentServices(t, t.Context())
	const hostID = store.LocalHostID
	const target = 7
	if _, err := fixture.db.SetHostMaxOperations(t.Context(), hostID, target); err != nil {
		t.Fatal(err)
	}
	const oldImage, currentImage = "workspace:old", "workspace:current"
	if _, err := fixture.db.Create(t.Context(), store.Workspace{
		ID: store.ID(), Name: "old", Image: oldImage, Branch: "main",
		Desired: "stopped", CPU: 100, Memory: 256,
	}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetWorkspaceImage(t.Context(), currentImage); err != nil {
		t.Fatal(err)
	}
	inventory := agentlink.Inventory{
		Version: "2.5.0", MaxConcurrent: 3,
		Images: []agentlink.ImageStatus{{Image: "workspace:obsolete", Ready: true}},
	}
	configuredImages := []string{currentImage, oldImage}
	beforeImages := []store.ImageStatus{{Image: "workspace:obsolete", Ready: true}}
	afterImages := []store.ImageStatus{{Image: currentImage, Ready: true}, {Image: oldImage, Ready: false}}
	// Drive the v1 wire protocol a frame at a time so a second heartbeat can
	// be sent before the first configuration exchange receives its result.
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	desc := &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}
	const service = "/cloudex.agent.v1.AgentLink/"
	control, err := fixture.conn.NewStream(ctx, desc, service+"Connect")
	if err != nil {
		t.Fatal(err)
	}
	sendAgentTestFrame(t, control, "heartbeat", inventory)
	var token string
	recvAgentTestFrame(t, control, "dispatch", &token)
	exchange, err := fixture.conn.NewStream(ctx, desc, service+"Exchange")
	if err != nil {
		t.Fatal(err)
	}
	sendAgentTestFrame(t, exchange, "bind", token)
	var req struct {
		Method string                `json:"method"`
		Config agentlink.AgentConfig `json:"config"`
	}
	recvAgentTestFrame(t, exchange, "request", &req)
	if req.Method != "configure" || req.Config.MaxConcurrent != target ||
		!slices.Equal(req.Config.Images, configuredImages) {
		t.Fatalf("heartbeat dispatched %+v, want configure limit %d with images %v", req, target, configuredImages)
	}
	before, err := fixture.db.Host(ctx, hostID)
	if err != nil || before.MaxOperations != 3 || before.MaxOperationsTarget != target || !slices.Equal(before.Images, beforeImages) {
		t.Fatalf("Host before configuration: %+v, %v", before, err)
	}
	inventory.MaxConcurrent = target
	inventory.Images = []agentlink.ImageStatus{{Image: oldImage}, {Image: currentImage, Ready: true}}
	sendAgentTestFrame(t, control, "heartbeat", inventory)
	// The test's deadline precedes the server-side configure deadline. A
	// synchronous configure call would still be waiting for the result.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		host, err := fixture.db.Host(ctx, hostID)
		if err != nil {
			t.Fatal(err)
		}
		if host.MaxOperations == target && slices.Equal(host.Images, afterImages) {
			break
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("the following heartbeat was not recorded while configure awaited its result")
		}
	}
	sendAgentTestFrame(t, exchange, "result", nil)
	var end wrapperspb.BytesValue
	if err := exchange.RecvMsg(&end); !errors.Is(err, io.EOF) {
		t.Fatalf("configuration exchange did not finish after its result: %v", err)
	}
}

type agentTestFrame struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value"`
}

func sendAgentTestFrame(t *testing.T, stream grpc.ClientStream, kind string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(agentTestFrame{Kind: kind, Value: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.Bytes(frame)); err != nil {
		t.Fatal(err)
	}
}

func recvAgentTestFrame(t *testing.T, stream grpc.ClientStream, kind string, value any) {
	t.Helper()
	var msg wrapperspb.BytesValue
	if err := stream.RecvMsg(&msg); err != nil {
		t.Fatal(err)
	}
	var frame agentTestFrame
	if err := json.Unmarshal(msg.Value, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Kind != kind {
		t.Fatalf("received %q frame, want %q", frame.Kind, kind)
	}
	if err := json.Unmarshal(frame.Value, value); err != nil {
		t.Fatal(err)
	}
}
