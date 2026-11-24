package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	*control.Control
	Origin           string
	AgentCertificate string
	DisconnectAgent  func(string)
	hostModeMu       sync.Mutex
	hostModeChanges  map[string]*hostModeChange
}

func (s *Server) Handler(web http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, protectConsole)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, r, 200, map[string]string{"status": "ok"}) })
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.apiHeaders, s.origin)
		r.Get("/auth/setup", s.setupStatus)
		r.Post("/auth/setup", s.setup)
		r.Post("/auth/login", s.login)
		r.Group(func(r chi.Router) {
			r.Use(s.authorize)
			r.Get("/auth/session", s.session)
			r.Post("/auth/logout", s.logout)
			r.Put("/auth/account", s.updateAccount)
			r.Get("/settings", s.settings)
			r.Put("/settings/access", s.setAccessSettings)
			r.Get("/settings/ssh-keys", s.sshKeys)
			r.Put("/settings/ssh-keys", s.setSSHKeys)
			r.Put("/settings/workspace-image", s.setWorkspaceImage)
			r.Get("/templates", s.templates)
			r.Get("/hosts", s.hosts)
			r.Post("/hosts", s.addHost)
			r.Route("/hosts/{host}", func(r chi.Router) {
				r.Get("/", s.getHost)
				r.Put("/mode", s.setHostMode)
				r.Post("/enrollment", s.renewHostEnrollment)
				r.Get("/capacity", s.hostCapacity)
				r.Put("/capacity", s.setHostCapacity)
				r.Put("/max-operations", s.setHostMaxOperations)
				r.Get("/containers", s.containersForHost)
				r.Get("/images", s.imagesForHost)
				r.Get("/operations", s.operationsForHost)
				r.Get("/reservations", s.reservationsForHost)
			})
			r.Get("/workspaces", s.workspaces)
			r.Post("/workspaces", s.createWorkspace)
			r.Route("/workspaces/{workspace}", func(r chi.Router) {
				r.Get("/", s.workspace)
				r.Delete("/", s.operation("delete"))
				for _, kind := range []string{"start", "stop", "rebuild", "reset"} {
					r.Post("/"+kind, s.operation(kind))
				}
				r.Get("/containers", s.history)
				r.Get("/inspect", s.inspect)
				r.Get("/metrics", s.metrics)
				r.Get("/logs", s.logs)
				r.Get("/git", s.git)
				r.Get("/operations", s.operations)
				r.Get("/terminal", s.terminal)
				r.Get("/access", s.access)
				r.Get("/codex-config", s.getCodexConfig)
				r.Put("/codex-config", s.setCodexConfig)
				r.Get("/repository-credential", s.getRepositoryCredential)
				r.Put("/repository-credential", s.putRepositoryCredential)
				r.Delete("/repository-credential", s.deleteRepositoryCredential)
			})
			r.Get("/operations", s.operations)
			r.Get("/operations/{operation}", s.getOperation)
			r.Post("/operations/{operation}/cancel", s.cancelOperation)
		})
	})
	r.Handle("/*", web)
	return r
}

func protectConsole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) origin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != "GET" && r.Method != "HEAD") || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			if r.Header.Get("Origin") != s.Origin {
				problem(w, r, 403, "ORIGIN_REJECTED", "Request origin is not allowed.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func reply(w http.ResponseWriter, r *http.Request, status int, value any) {
	respond(w, r, status, map[string]any{"data": value, "request_id": middleware.GetReqID(r.Context())})
}
func problem(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	respond(w, r, status, map[string]any{"error": map[string]string{"code": code, "message": message}, "request_id": middleware.GetReqID(r.Context())})
}
func bad(w http.ResponseWriter, r *http.Request, message string) {
	problem(w, r, 400, "INVALID_REQUEST", message)
}
func failure(w http.ResponseWriter, r *http.Request, e error) {
	var capacity *store.CapacityError
	switch {
	case errors.Is(e, store.ErrMissing), errors.Is(e, runtime.ErrMissing):
		problem(w, r, 404, "NOT_FOUND", "The requested resource was not found.")
	case errors.Is(e, store.ErrBusy):
		problem(w, r, 409, "WORKSPACE_BUSY", "Wait for active work to finish, or cancel it before retrying.")
	case errors.Is(e, store.ErrConflict):
		problem(w, r, 409, "CONFLICT", "The name or idempotency key is already in use.")
	case errors.Is(e, store.ErrWorkspacePaused):
		problem(w, r, 409, "WORKSPACE_PAUSED", e.Error())
	case errors.As(e, &capacity):
		respond(w, r, http.StatusConflict, map[string]any{
			"error":      map[string]any{"code": "INSUFFICIENT_HOST_CAPACITY", "message": "Not enough " + capacity.Resource + " capacity on the selected Host.", "details": capacity},
			"request_id": middleware.GetReqID(r.Context()),
		})
	case errors.Is(e, store.ErrHostOffline):
		problem(w, r, 409, "HOST_OFFLINE", "The selected Host is unreachable or Docker is unavailable.")
	case errors.Is(e, store.ErrHostCordoned):
		problem(w, r, 409, "HOST_CORDONED", "The selected Host is cordoned.")
	case errors.Is(e, store.ErrHostDisabled):
		problem(w, r, 409, "HOST_DISABLED", "The selected Host is disabled.")
	case errors.Is(e, store.ErrHostIncompatible):
		problem(w, r, 409, "HOST_INCOMPATIBLE", "The selected Host does not have the required Workspace image.")
	case errors.Is(e, store.ErrHostMode), errors.Is(e, store.ErrInvalidEnrollment):
		bad(w, r, e.Error())
	default:
		slog.Error("API request failed", "request_id", middleware.GetReqID(r.Context()), "error", e)
		problem(w, r, 500, "INTERNAL", "The request could not be completed.")
	}
}
func dockerFailure(w http.ResponseWriter, r *http.Request, e error) {
	// The link reports a request that ran out of time as unavailable too; a
	// slow call on a connected Host is not an unreachable Host.
	if errors.Is(e, runtime.ErrHostUnavailable) && !errors.Is(e, context.DeadlineExceeded) {
		problem(w, r, http.StatusServiceUnavailable, "HOST_UNREACHABLE", "The workspace Host Agent is unreachable.")
		return
	}
	if errors.Is(e, store.ErrMissing) || errors.Is(e, runtime.ErrMissing) || errors.Is(e, store.ErrBusy) {
		failure(w, r, e)
		return
	}
	slog.Warn("Docker call failed", "request_id", middleware.GetReqID(r.Context()), "error", e)
	problem(w, r, 503, "RUNTIME_UNAVAILABLE", "Docker could not complete this request. Check the host or workspace state.")
}
func parse(w http.ResponseWriter, r *http.Request, target any, optional bool) bool {
	return parseWithLimit(w, r, target, optional, 96*1024)
}
func parseWithLimit(w http.ResponseWriter, r *http.Request, target any, optional bool, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(target); e != nil {
		if optional && errors.Is(e, io.EOF) {
			return true
		}
		bad(w, r, "Expected a valid JSON request body.")
		return false
	}
	if e := d.Decode(new(any)); !errors.Is(e, io.EOF) {
		bad(w, r, "Expected one JSON object.")
		return false
	}
	return true
}
func send[T any](w http.ResponseWriter, r *http.Request, data T, e error) {
	if e != nil {
		failure(w, r, e)
		return
	}
	reply(w, r, 200, data)
}
func dockerSend[T any](w http.ResponseWriter, r *http.Request, data T, e error) {
	if e != nil {
		dockerFailure(w, r, e)
		return
	}
	reply(w, r, 200, data)
}
func fingerprint(v any) string           { b, _ := json.Marshal(v); return digest(string(b)) }
func digest(v string) string             { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func workspaceID(r *http.Request) string { return chi.URLParam(r, "workspace") }
func shortCall(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 10*time.Second)
}
