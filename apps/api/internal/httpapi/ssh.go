package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
)

type sshKeySettings struct {
	PublicKeys []string `json:"public_keys"`
}

func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	view, err := s.Control.Access(ctx, workspaceID(r))
	send(w, r, view, err)
}

func (s *Server) sshKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.SSHKeys(r.Context())
	send(w, r, sshKeySettings{PublicKeys: keys}, err)
}

func (s *Server) setSSHKeys(w http.ResponseWriter, r *http.Request) {
	var in sshKeySettings
	if !parse(w, r, &in, false) {
		return
	}
	if in.PublicKeys == nil {
		bad(w, r, "Send public_keys as an array.")
		return
	}
	keys, err := remote.Keys(in.PublicKeys)
	if err != nil {
		bad(w, r, err.Error())
		return
	}
	if err := s.Store.SetSSHKeys(r.Context(), keys); err != nil {
		failure(w, r, err)
		return
	}
	// Deliver a committed change even if the client disconnects after saving.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	if err := s.Control.SyncSSHKeys(ctx); err != nil {
		slog.Warn("update SSH keys on running workspaces", "error", err)
		problem(w, r, http.StatusServiceUnavailable, "SSH_KEY_SYNC_FAILED", "Public keys are saved, but could not be sent to all running workspaces. Retry the update.")
		return
	}
	send(w, r, sshKeySettings{PublicKeys: keys}, nil)
}
