package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type heldGatewayInstall struct {
	installed chan struct{}
	release   chan struct{}
}

type lifecycleGatewayEngine struct {
	*runtime.Docker
	offline atomic.Bool
	hold    atomic.Pointer[heldGatewayInstall]
	tokens  sync.Map
}

func (e *lifecycleGatewayEngine) Inspect(ctx context.Context, spec runtime.Spec) (runtime.Container, error) {
	if e.offline.Load() {
		return runtime.Container{}, runtime.ErrHostUnavailable
	}
	return e.Docker.Inspect(ctx, spec)
}

func (e *lifecycleGatewayEngine) InstallGatewayCredential(ctx context.Context, spec runtime.Spec, instance runtime.GatewayInstance, install runtime.GatewayCredentialInstall) (runtime.GatewayCredentialConfirmation, error) {
	confirmation, err := e.Docker.InstallGatewayCredential(ctx, spec, instance, install)
	if err != nil {
		return confirmation, err
	}
	e.tokens.Store(confirmation.Digest, install.Token)
	if hold := e.hold.Swap(nil); hold != nil {
		close(hold.installed)
		select {
		case <-hold.release:
		case <-ctx.Done():
			return confirmation, ctx.Err()
		}
	}
	return confirmation, nil
}

func TestDockerGatewayCredentialLifecycle(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_DOCKER") != "1" {
		t.Skip("set CLOUDEX_TEST_DOCKER=1 with an isolated daemon and built Workspace image")
	}
	network := os.Getenv("CLOUDEX_TEST_SSH_NETWORK")
	if network == "" {
		t.Fatal("CLOUDEX_TEST_SSH_NETWORK must identify the disposable test bridge")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "lifecycle.sqlite")
	open := func() *store.Store {
		t.Helper()
		db, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open()
	instance, err := db.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	docker, err := runtime.NewDocker(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	if err := docker.EnsureSSHNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	engine := &lifecycleGatewayEngine{Docker: docker}
	ssh, err := remote.NewConfig("127.0.0.1", "2222", network)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(make([]byte, 32))
	key, _ := cipher.NewGCM(block)
	fake := newGatewayFixture(t, codexupstream.ChannelCodex, fixtureFault{})
	start := func(db *store.Store) (*codexgateway.Service, *control.Control, func(), func()) {
		t.Helper()
		worker, stop := context.WithCancel(ctx)
		service, err := codexgateway.New(worker, codexgateway.Config{
			Store: db, Key: key, Origin: "https://gateway.test", RedirectURI: "http://127.0.0.1:1455/auth/callback",
			Clients: []*codexupstream.Client{fake.adapter()},
		})
		if err != nil {
			stop()
			t.Fatal(err)
		}
		controller := &control.Control{Store: db, Engine: engine, Gateway: service, SSH: ssh}
		var wait sync.WaitGroup
		startWorker := sync.OnceFunc(func() {
			wait.Go(func() {
				if err := controller.Run(worker); err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			})
		})
		shutdown := sync.OnceFunc(func() { stop(); wait.Wait(); service.Close() })
		t.Cleanup(shutdown)
		return service, controller, startWorker, shutdown
	}
	service, controller, startWorker, shutdown := start(db)
	startWorker()
	const session = "lifecycle-admin-fixture"
	authorization, err := service.BeginAuthorization(ctx, session, codexupstream.ChannelCodex, "")
	if err != nil {
		t.Fatal(err)
	}
	state, code, issued := fake.issueCode(authorization.AuthorizationURL)
	if err := service.CompleteAuthorization(ctx, state, code, issued, ""); err != nil {
		t.Fatal(err)
	}
	grant, err := service.ConfirmAuthorization(ctx, session, authorization.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := store.Workspace{ID: store.ID(), Name: "gateway-lifecycle", Image: store.DefaultWorkspaceImage,
		Branch: "main", CPU: 500, Memory: 512, Desired: "running"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for generation := 1; generation <= 3; generation++ {
			if err := docker.Remove(ctx, runtime.Spec{WorkspaceID: w.ID, Generation: generation}); err != nil {
				t.Error(err)
			}
		}
		for revision := 0; revision <= 1; revision++ {
			if err := docker.Purge(ctx, runtime.Spec{WorkspaceID: w.ID, ProjectVolumeRevision: revision}, true); err != nil {
				t.Error(err)
			}
		}
	})
	await := func(label string, condition func() bool) {
		t.Helper()
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for !condition() {
			select {
			case <-timer.C:
				t.Fatalf("timed out waiting for %s", label)
			case <-ctx.Done():
				t.Fatalf("%s: %v", label, ctx.Err())
			case <-tick.C:
			}
		}
	}
	waitOp := func(op store.Operation) {
		t.Helper()
		await(op.Type, func() bool {
			current, err := db.Operation(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status == "failed" {
				t.Fatalf("%s lifecycle failed: %s", op.Type, current.Error)
			}
			return current.Status == "succeeded"
		})
	}
	gatewayStatus := func(credential store.GatewayCredential, status int) {
		t.Helper()
		token, ok := engine.tokens.Load(credential.Digest)
		if !ok {
			t.Fatalf("expected a Host installation for sequence %d", credential.Sequence)
		}
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/g/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+token.(string))
		response := httptest.NewRecorder()
		service.GatewayHandler().ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("gateway sequence %d returned %d, expected %d", credential.Sequence, response.Code, status)
		}
	}
	active := func() store.GatewayCredential {
		t.Helper()
		var credential store.GatewayCredential
		await("active gateway credential", func() bool {
			view, err := service.Binding(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Readiness != "ready" {
				return false
			}
			credential, err = db.WorkspaceCredential(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			return credential.Status == store.CredentialActive
		})
		gatewayStatus(credential, http.StatusOK)
		return credential
	}
	enqueue := func(kind string, purge bool) store.Operation {
		t.Helper()
		op, err := service.Enqueue(ctx, w.ID, kind, purge, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	op, err := service.CreateWorkspace(ctx, w, "", "", nil, grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitOp(op)
	first := active()
	engine.offline.Store(true)
	if err := controller.ReconcileGateway(ctx, w); err != nil {
		t.Fatal(err)
	}
	gatewayStatus(first, http.StatusServiceUnavailable)
	engine.offline.Store(false)
	if err := controller.ReconcileGateway(ctx, w); err != nil {
		t.Fatal(err)
	}
	if recovered := active(); recovered != first {
		t.Fatal("reconnected Host did not confirm the existing instance credential")
	}
	engine.offline.Store(true)
	shutdown()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	service, controller, startWorker, _ = start(db)
	gatewayStatus(first, http.StatusServiceUnavailable)
	engine.offline.Store(false)
	if err := controller.ReconcileGateway(ctx, w); err != nil {
		t.Fatal(err)
	}
	if recovered := active(); recovered != first {
		t.Fatal("restarted control plane did not reverify the existing instance credential")
	}

	hold := &heldGatewayInstall{installed: make(chan struct{}), release: make(chan struct{})}
	engine.hold.Store(hold)
	if err := service.RotateCredential(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	gatewayStatus(first, http.StatusUnauthorized)
	done := make(chan error, 1)
	go func() { done <- controller.ReconcileGateway(ctx, w) }()
	select {
	case <-hold.installed:
	case err := <-done:
		t.Fatalf("replacement installation did not reach the Host: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pending, err := db.WorkspaceCredential(ctx, w.ID)
	if err != nil || pending.Status != store.CredentialPending {
		t.Fatalf("missing pending confirmation: %v", err)
	}
	gatewayStatus(pending, http.StatusUnauthorized)
	stop := enqueue("stop", false)
	close(hold.release)
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrBusy) && !errors.Is(err, store.ErrConflict) {
			t.Fatalf("late installation result was processed after accepting Stop: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	startWorker()
	waitOp(stop)
	gatewayStatus(pending, http.StatusUnauthorized)
	waitOp(enqueue("start", false))
	started := active()
	if started.DockerID != first.DockerID || started.StartedAt == first.StartedAt || started.Generation != first.Generation || started.Sequence <= pending.Sequence {
		t.Fatal("Start did not issue a distinct startup credential for the same container")
	}
	for _, operation := range []string{"rebuild", "reset"} {
		op := enqueue(operation, false)
		gatewayStatus(started, http.StatusUnauthorized)
		waitOp(op)
		next := active()
		if next.DockerID == started.DockerID || next.Generation != started.Generation+1 || next.Sequence <= started.Sequence {
			t.Fatalf("%s did not issue a credential for the new container generation", operation)
		}
		started = next
	}
	waitOp(enqueue("delete", true))
	gatewayStatus(started, http.StatusUnauthorized)
	if _, err := db.Binding(ctx, w.ID); !errors.Is(err, store.ErrMissing) {
		t.Fatalf("Delete retained the Workspace binding: %v", err)
	}
	t.Log("Confirmed Create, failed Host verification and recovery, control-plane restart, rotation, late confirmation after Stop, Start, Rebuild, Reset and Delete against the isolated Docker daemon")
}
