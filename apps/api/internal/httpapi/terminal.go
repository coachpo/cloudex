package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/coder/websocket"
)

const terminalIOTimeout = 10 * time.Second

var errTerminalSessionRevoked = errors.New("Sign in to open a workspace shell")

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.Origin {
		problem(w, r, 403, "ORIGIN_REJECTED", "Request origin is not allowed.")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		w.Header().Set("Upgrade", "websocket")
		problem(w, r, http.StatusUpgradeRequired, "UPGRADE_REQUIRED", "Open this endpoint with WebSocket.")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	session := ctx.Value(sessionKey{}).(string)
	checkSession := func(ctx context.Context) error {
		_, err := s.Store.Session(ctx, session)
		if errors.Is(err, store.ErrMissing) {
			return errTerminalSessionRevoked
		}
		return err
	}
	dial, stop := context.WithTimeout(ctx, 8*time.Second)
	ws, record, e := s.Store.ReadyContainer(dial, workspaceID(r))
	if e != nil {
		stop()
		failure(w, r, e)
		return
	}
	current, e := s.Inspect(dial, ws)
	stop()
	if e != nil {
		dockerFailure(w, r, e)
		return
	}
	if current.ID != record.ID || current.State != "running" {
		problem(w, r, 409, "WORKSPACE_BUSY", "Workspace is not running.")
		return
	}
	conn, e := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if e != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 * 1024)
	admission, release := context.WithTimeout(ctx, 8*time.Second)
	var tty runtime.Terminal
	e = s.Store.AdmitTerminal(admission, ws.ID, ws.Generation, func() error {
		if err := checkSession(admission); err != nil {
			return err
		}
		opening := ctx
		opened := func() {}
		if s.Agents == nil {
			opening, opened = context.WithTimeout(ctx, 8*time.Second)
		}
		defer opened()
		var err error
		tty, err = s.EngineFor(ws.HostID).Terminal(opening, current.ID)
		if err != nil {
			return err
		}
		return checkSession(admission)
	})
	release()
	if e != nil {
		if tty != nil {
			_ = tty.Close()
		}
		switch {
		case errors.Is(e, errTerminalSessionRevoked):
			_ = conn.Close(websocket.StatusPolicyViolation, e.Error())
		case errors.Is(e, store.ErrBusy):
			_ = conn.Close(websocket.StatusPolicyViolation, "Workspace is stopped or changing")
		default:
			_ = conn.Close(websocket.StatusInternalError, "Could not open workspace shell")
		}
		return
	}
	defer tty.Close()
	go func() {
		defer cancel()
		buf := make([]byte, 16*1024)
		for {
			n, e := tty.Read(buf)
			if n > 0 {
				write, end := context.WithTimeout(ctx, terminalIOTimeout)
				send := conn.Write(write, websocket.MessageBinary, buf[:n])
				end()
				if send != nil {
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			check, end := context.WithTimeout(ctx, 5*time.Second)
			err := checkSession(check)
			end()
			if err != nil {
				cancel()
				return
			}
		}
	}()
	unblock := context.AfterFunc(ctx, func() { tty.Close() })
	defer unblock()
	for {
		kind, data, e := conn.Read(ctx)
		if e != nil {
			return
		}
		if kind == websocket.MessageBinary {
			// Bound PTY backpressure while this goroutine cannot read WebSocket frames.
			if e = tty.SetWriteDeadline(time.Now().Add(terminalIOTimeout)); e != nil {
				return
			}
			if _, e = tty.Write(data); e != nil {
				return
			}
			continue
		}
		var msg struct {
			Type       string
			Cols, Rows uint
		}
		if e = json.Unmarshal(data, &msg); e != nil {
			continue
		}
		switch msg.Type {
		case "resize":
			if msg.Cols > 0 && msg.Cols <= 500 && msg.Rows > 0 && msg.Rows <= 300 {
				resize, end := context.WithTimeout(ctx, 3*time.Second)
				_ = tty.Resize(resize, msg.Cols, msg.Rows)
				end()
			}
		case "close":
			return
		}
	}
}
