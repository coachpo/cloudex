package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentauth"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/go-chi/chi/v5"
)

func hostID(r *http.Request) string { return chi.URLParam(r, "host") }

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.Store.Hosts(r.Context())
	send(w, r, hosts, err)
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.Store.Host(r.Context(), hostID(r))
	send(w, r, host, err)
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), hostID(r)
	unlock, err := s.lockHostMode(ctx, id)
	if err != nil {
		failure(w, r, err)
		return
	}
	defer unlock()
	connectionType, err := s.Store.DeleteHost(ctx, id)
	switch {
	case errors.Is(err, store.ErrLocalHostProtected):
		problem(w, r, 409, "HOST_LOCAL_PROTECTED", "The built-in Host cannot be deleted.")
	case errors.Is(err, store.ErrHostHasWorkspaces):
		problem(w, r, 409, "HOST_HAS_WORKSPACES", "Delete the Workspaces on this Host first.")
	case errors.Is(err, store.ErrHostHasPendingOps):
		problem(w, r, 409, "HOST_HAS_PENDING_OPERATIONS", "Wait for this Host's pending operations to finish, or cancel eligible operations before retrying.")
	case err != nil:
		failure(w, r, err)
	default:
		if connectionType == "ssh" {
			if s.SSHHosts != nil {
				s.SSHHosts.Removed(id)
			}
		} else if s.DisconnectAgent != nil {
			s.DisconnectAgent(id)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) hostCapacity(w http.ResponseWriter, r *http.Request) {
	host, err := s.Store.Host(r.Context(), hostID(r))
	if err != nil {
		failure(w, r, err)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{
		"host_id": host.ID, "status": host.Status, "scheduling_state": host.Scheduling, "last_seen": host.LastSeen,
		"allocatable": host.Capacity, "reserved": host.Reserved, "available": host.Available,
	})
}

func (s *Server) setHostCapacity(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disk *int64 `json:"disk_bytes"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	if in.Disk == nil || *in.Disk < 0 {
		problem(w, r, http.StatusBadRequest, "INVALID_DISK_BYTES", "Enter a nonnegative disk reservation capacity.")
		return
	}
	host, err := s.Store.SetHostDisk(r.Context(), hostID(r), *in.Disk)
	send(w, r, host, err)
}

func (s *Server) setHostMaxOperations(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MaxOperations int `json:"max_operations"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	if in.MaxOperations < 1 || in.MaxOperations > 100 {
		problem(w, r, http.StatusBadRequest, "INVALID_MAX_OPERATIONS", "Enter a concurrent operation limit from 1 to 100.")
		return
	}
	host, err := s.Store.SetHostMaxOperations(r.Context(), hostID(r), in.MaxOperations)
	send(w, r, host, err)
}

func (s *Server) addHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !workspaceName.MatchString(in.Name) {
		bad(w, r, "Use 1–64 letters, digits, spaces, dots, underscores or hyphens for the Host name.")
		return
	}
	s.issueEnrollment(w, r, in.Name)
}

func (s *Server) renewHostEnrollment(w http.ResponseWriter, r *http.Request) {
	s.issueEnrollment(w, r, "")
}

func (s *Server) issueEnrollment(w http.ResponseWriter, r *http.Request, name string) {
	if s.AgentCertificate == "" {
		problem(w, r, http.StatusServiceUnavailable, "AGENT_ENROLLMENT_UNAVAILABLE", "Host Agent enrollment is unavailable.")
		return
	}
	settings, err := s.Store.Settings(r.Context())
	if err != nil {
		failure(w, r, err)
		return
	}
	var b [32]byte
	rand.Read(b[:])
	token := base64.RawURLEncoding.EncodeToString(b[:])
	hash, expires := agentauth.TokenHash(token), time.Now().UTC().Truncate(time.Second).Add(15*time.Minute)
	var host store.Host
	status := http.StatusCreated
	if name != "" {
		host, err = s.Store.AddHost(r.Context(), name, hash, expires)
	} else {
		host, err = s.Store.RenewEnrollment(r.Context(), hostID(r), hash, expires)
		status = http.StatusOK
	}
	if err != nil {
		failure(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/hosts/"+host.ID)
	reply(w, r, status, map[string]any{
		"host": host, "enrollment_token": token, "expires_at": expires,
		"agent_address": settings.AgentPublicAddress, "ca_certificate": s.AgentCertificate,
	})
}

func (s *Server) setHostMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	host, err := s.applyHostMode(r.Context(), hostID(r), in.Mode)
	send(w, r, host, err)
}

// Keep disabling a Host and invalidating its current Agent link in order with
// later mode changes. A late invalidation must not close a resumed Host's link.
func (s *Server) applyHostMode(ctx context.Context, id, mode string) (store.Host, error) {
	unlock, err := s.lockHostMode(ctx, id)
	if err != nil {
		return store.Host{}, err
	}
	defer unlock()
	previous, err := s.Store.Host(ctx, id)
	if err != nil {
		return store.Host{}, err
	}
	host, err := s.Store.SetHostScheduling(ctx, id, mode)
	if err != nil {
		return host, err
	}
	if host.ConnectionType == "ssh" {
		if s.SSHHosts != nil && (previous.Scheduling == "disabled") != (host.Scheduling == "disabled") {
			s.SSHHosts.Changed(id)
		}
	} else if mode == "disabled" && s.DisconnectAgent != nil {
		s.DisconnectAgent(host.ID)
	}
	return host, err
}

type hostModeChange struct {
	permit chan struct{}
	users  int
}

func (s *Server) lockHostMode(ctx context.Context, id string) (func(), error) {
	s.hostModeMu.Lock()
	if s.hostModeChanges == nil {
		s.hostModeChanges = make(map[string]*hostModeChange)
	}
	change := s.hostModeChanges[id]
	if change == nil {
		change = &hostModeChange{permit: make(chan struct{}, 1)}
		s.hostModeChanges[id] = change
	}
	change.users++
	s.hostModeMu.Unlock()

	unref := func() {
		s.hostModeMu.Lock()
		defer s.hostModeMu.Unlock()
		change.users--
		if change.users == 0 {
			delete(s.hostModeChanges, id)
		}
	}
	select {
	case change.permit <- struct{}{}:
		unlock := func() {
			<-change.permit
			unref()
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

func (s *Server) containersForHost(w http.ResponseWriter, r *http.Request) {
	s.withHostEngine(w, r, func(engine runtime.Engine) {
		ctx, cancel := shortCall(r)
		defer cancel()
		v, err := engine.Containers(ctx)
		dockerSend(w, r, v, err)
	})
}

func (s *Server) imagesForHost(w http.ResponseWriter, r *http.Request) {
	s.withHostEngine(w, r, func(engine runtime.Engine) {
		ctx, cancel := shortCall(r)
		defer cancel()
		v, err := engine.Images(ctx)
		dockerSend(w, r, v, err)
	})
}

func (s *Server) withHostEngine(w http.ResponseWriter, r *http.Request, fn func(runtime.Engine)) {
	host, err := s.Store.Host(r.Context(), hostID(r))
	if err != nil {
		failure(w, r, err)
		return
	}
	fn(s.EngineFor(host.ID))
}

func (s *Server) operationsForHost(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Host(r.Context(), hostID(r)); err != nil {
		failure(w, r, err)
		return
	}
	v, err := s.Store.HostOperations(r.Context(), hostID(r))
	send(w, r, v, err)
}

func (s *Server) reservationsForHost(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Host(r.Context(), hostID(r)); err != nil {
		failure(w, r, err)
		return
	}
	v, err := s.Store.HostReservations(r.Context(), hostID(r))
	send(w, r, v, err)
}
