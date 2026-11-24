package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coder/websocket"
)

func TestTerminalRequiresWebSocketUpgrade(t *testing.T) {
	a := newTestAPI(t, &control.Control{Store: newDB(t)})
	a.login()

	res := a.raw(http.MethodGet, "/workspaces/missing/terminal", testOrigin, "", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusUpgradeRequired || res.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("plain terminal request: status=%d upgrade=%q", res.StatusCode, res.Header.Get("Upgrade"))
	}
}

// shellRecorder is a PTY that counts its input and stays open until closed.
type shellRecorder struct {
	mu       sync.Mutex
	received int
	closed   chan struct{}
	once     sync.Once
}

func (s *shellRecorder) Read([]byte) (int, error) { <-s.closed; return 0, io.EOF }
func (s *shellRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received += len(p)
	return len(p), nil
}
func (s *shellRecorder) Close() error                             { s.once.Do(func() { close(s.closed) }); return nil }
func (s *shellRecorder) Resize(context.Context, uint, uint) error { return nil }
func (s *shellRecorder) SetWriteDeadline(time.Time) error         { return nil }
func (s *shellRecorder) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

type shellEngine struct {
	inspectedContainer
	shell runtime.Terminal
}

func (e shellEngine) Terminal(context.Context, string) (runtime.Terminal, error) { return e.shell, nil }

func dialTerminal(ctx context.Context, a testAPI, workspace string) (*websocket.Conn, error) {
	u, _ := url.Parse(a.server.URL)
	header := http.Header{"Origin": {testOrigin}}
	for _, ck := range a.client.Jar.Cookies(u) {
		header.Add("Cookie", ck.String())
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.server.URL, "http")+"/api/v1/workspaces/"+workspace+"/terminal", &websocket.DialOptions{HTTPHeader: header})
	return conn, err
}

func TestTerminalEndsShellOnMessageOverLimit(t *testing.T) {
	const limit = 64 * 1024
	db := newDB(t)
	w := seedWorkspace(t, db, "running", nil)
	shell := &shellRecorder{closed: make(chan struct{})}
	a := newTestAPI(t, &control.Control{Store: db, Engine: shellEngine{
		inspectedContainer{container: runtime.Container{ID: "fixture-container", State: "running"}}, shell}})
	a.login()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := dialTerminal(ctx, a, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte("a"), limit)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "largest allowed message to reach the shell", func() bool { return shell.size() == limit })
	_ = conn.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte("b"), limit+1))
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("oversized message was not refused as too big: %v", err)
	}
	select {
	case <-shell.closed:
	case <-ctx.Done():
		t.Fatal("oversized message left the shell running")
	}
	if got := shell.size(); got != limit {
		t.Fatalf("shell received %d bytes, want only the %d allowed", got, limit)
	}
}

type terminalAdmissionEngine struct {
	shellEngine
	inspecting    chan struct{}
	resumeInspect chan struct{}
	opening       chan struct{}
	resumeOpen    chan struct{}
	opened        atomic.Bool
}

func (e *terminalAdmissionEngine) Inspect(ctx context.Context, _ runtime.Spec) (runtime.Container, error) {
	close(e.inspecting)
	select {
	case <-e.resumeInspect:
		return e.container, nil
	case <-ctx.Done():
		return runtime.Container{}, ctx.Err()
	}
}

func (e *terminalAdmissionEngine) Terminal(ctx context.Context, id string) (runtime.Terminal, error) {
	e.opened.Store(true)
	if e.opening != nil {
		close(e.opening)
		select {
		case <-e.resumeOpen:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.shellEngine.Terminal(ctx, id)
}

func terminalCloseResult(ctx context.Context, a testAPI, workspace string) <-chan error {
	result := make(chan error, 1)
	go func() {
		conn, err := dialTerminal(ctx, a, workspace)
		if err == nil {
			defer conn.CloseNow()
			_, _, err = conn.Read(ctx)
		}
		result <- err
	}()
	return result
}

func TestTerminalRechecksLifecycleAdmissionAfterInspect(t *testing.T) {
	db := newDB(t)
	w := seedWorkspace(t, db, "running", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	engine := &terminalAdmissionEngine{
		shellEngine: shellEngine{inspectedContainer: inspectedContainer{container: runtime.Container{
			ID: "fixture-container", State: "running",
		}}, shell: &shellRecorder{closed: make(chan struct{})}},
		inspecting:    make(chan struct{}),
		resumeInspect: make(chan struct{}),
	}
	a := newTestAPI(t, &control.Control{Store: db, Engine: engine})
	a.login()
	result := terminalCloseResult(ctx, a, w.ID)
	select {
	case <-engine.inspecting:
	case <-ctx.Done():
		t.Fatal("terminal request did not reach runtime inspection")
	}
	res := a.raw(http.MethodPost, "/workspaces/"+w.ID+"/stop", testOrigin, "terminal-admission", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d", res.StatusCode)
	}
	close(engine.resumeInspect)
	if err := <-result; websocket.CloseStatus(err) != websocket.StatusPolicyViolation || engine.opened.Load() {
		t.Fatalf("terminal opened after a stop was queued: opened=%v, err=%v", engine.opened.Load(), err)
	}
}

func TestTerminalRejectsSessionRevokedDuringSetup(t *testing.T) {
	for _, stage := range []string{"inspect", "open"} {
		t.Run(stage, func(t *testing.T) {
			db := newDB(t)
			w := seedWorkspace(t, db, "running", nil)
			shell := &shellRecorder{closed: make(chan struct{})}
			engine := &terminalAdmissionEngine{
				shellEngine: shellEngine{inspectedContainer: inspectedContainer{container: runtime.Container{
					ID: "fixture-container", State: "running",
				}}, shell: shell},
				inspecting:    make(chan struct{}),
				resumeInspect: make(chan struct{}),
			}
			entered, resume := engine.inspecting, engine.resumeInspect
			if stage == "open" {
				close(engine.resumeInspect)
				engine.opening, engine.resumeOpen = make(chan struct{}), make(chan struct{})
				entered, resume = engine.opening, engine.resumeOpen
			}
			a := newTestAPI(t, &control.Control{Store: db, Engine: engine})
			a.login()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result := terminalCloseResult(ctx, a, w.ID)
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatalf("terminal did not reach %s", stage)
			}
			a.call(http.MethodPost, "/auth/logout", nil, http.StatusOK, nil)
			close(resume)
			if err := <-result; websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("terminal did not reject revoked session: %v", err)
			}
			if stage == "inspect" {
				if engine.opened.Load() {
					t.Fatal("terminal was allocated after the session was revoked")
				}
				return
			}
			select {
			case <-shell.closed:
			case <-ctx.Done():
				t.Fatal("terminal allocated during revocation was not closed")
			}
		})
	}
}

// stalledShell only unblocks its input on a simulated deadline or shell close.
type stalledShell struct {
	shellRecorder
	deadline time.Time
	writing  chan time.Time
	expire   chan struct{}
}

func (s *stalledShell) SetWriteDeadline(deadline time.Time) error {
	s.deadline = deadline
	return nil
}

func (s *stalledShell) Write([]byte) (int, error) {
	s.writing <- s.deadline
	select {
	case <-s.expire:
		return 0, os.ErrDeadlineExceeded
	case <-s.closed:
		return 0, io.ErrClosedPipe
	}
}

func TestTerminalClosesShellWhenInputWriteStalls(t *testing.T) {
	db := newDB(t)
	w := seedWorkspace(t, db, "running", nil)
	shell := &stalledShell{
		shellRecorder: shellRecorder{closed: make(chan struct{})},
		writing:       make(chan time.Time, 1),
		expire:        make(chan struct{}),
	}
	defer shell.Close()
	a := newTestAPI(t, &control.Control{Store: db, Engine: shellEngine{inspectedContainer: inspectedContainer{
		container: runtime.Container{ID: "fixture-container", State: "running"},
	}, shell: shell}})
	a.login()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := dialTerminal(ctx, a, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("command")); err != nil {
		t.Fatal(err)
	}
	select {
	case deadline := <-shell.writing:
		if remaining := time.Until(deadline); remaining <= 0 || remaining > terminalIOTimeout {
			t.Fatalf("expected a finite PTY input deadline, got %v", deadline)
		}
		close(shell.expire)
	case <-ctx.Done():
		t.Fatal("terminal input never reached the PTY")
	}
	select {
	case <-shell.closed:
	case <-ctx.Done():
		t.Fatal("timed-out PTY input left the shell open")
	}
	if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("timed-out PTY input left the connection open: %v", err)
	}
}
