package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type grantView struct {
	store.Grant
	WorkspaceIDs []string               `json:"workspace_ids"`
	Workspaces   []store.GrantWorkspace `json:"workspaces"`
}

func (s *Server) grantView(ctx context.Context, g store.Grant) (grantView, error) {
	workspaces, err := s.Store.GrantWorkspaces(ctx, g.ID)
	view := grantView{Grant: g, WorkspaceIDs: make([]string, 0, len(workspaces)), Workspaces: workspaces}
	for _, workspace := range workspaces {
		view.WorkspaceIDs = append(view.WorkspaceIDs, workspace.ID)
	}
	return view, err
}

func (s *Server) grants(w http.ResponseWriter, r *http.Request) {
	grants, err := s.Store.Grants(r.Context())
	if err != nil {
		failure(w, r, err)
		return
	}
	views := make([]grantView, 0, len(grants))
	for _, g := range grants {
		view, err := s.grantView(r.Context(), g)
		if err != nil {
			failure(w, r, err)
			return
		}
		views = append(views, view)
	}
	reply(w, r, http.StatusOK, map[string]any{"grants": views})
}

func (s *Server) beginGrantAuthorization(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Channel string `json:"channel"`
		GrantID string `json:"grant_id"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	result, err := s.GrantGateway.BeginAuthorization(r.Context(), sessionOwner(r), in.Channel, in.GrantID)
	if err != nil {
		grantFailure(w, r, err)
		return
	}
	reply(w, r, http.StatusCreated, result)
}

func sessionOwner(r *http.Request) string { return r.Context().Value(sessionKey{}).(string) }

func (s *Server) grantAuthorizationStatus(w http.ResponseWriter, r *http.Request) {
	result, err := s.GrantGateway.AuthorizationStatus(sessionOwner(r), chi.URLParam(r, "authorization"))
	send(w, r, result, err)
}

func (s *Server) confirmGrantAuthorization(w http.ResponseWriter, r *http.Request) {
	result, err := s.GrantGateway.ConfirmAuthorization(r.Context(), sessionOwner(r), chi.URLParam(r, "authorization"))
	if err != nil {
		grantFailure(w, r, err)
		return
	}
	view, err := s.grantView(r.Context(), result)
	send(w, r, view, err)
}

func (s *Server) disconnectGrant(w http.ResponseWriter, r *http.Request) {
	result, err := s.GrantGateway.DisconnectGrant(r.Context(), chi.URLParam(r, "grant"))
	if err != nil {
		grantFailure(w, r, err)
		return
	}
	view, err := s.grantView(r.Context(), result)
	send(w, r, view, err)
}

func (s *Server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "grant")
	if err := s.GrantGateway.DeleteGrant(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrGrantReferenced) {
			workspaces, err := s.Store.GrantWorkspaces(r.Context(), id)
			if err != nil {
				failure(w, r, err)
				return
			}
			ids := make([]string, 0, len(workspaces))
			for _, workspace := range workspaces {
				ids = append(ids, workspace.ID)
			}
			grantConflict(w, r, "grant_referenced", "Unbind the affected Workspaces before deleting this grant.", map[string]any{"workspace_ids": ids})
			return
		}
		grantFailure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) grantBinding(w http.ResponseWriter, r *http.Request) {
	result, err := s.GrantGateway.Binding(r.Context(), workspaceID(r))
	if err != nil {
		grantFailure(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(result.BindingRevision, 10)))
	reply(w, r, http.StatusOK, result)
}

func (s *Server) changeGrantBinding(w http.ResponseWriter, r *http.Request) {
	tag, err := strconv.Unquote(r.Header.Get("If-Match"))
	rev, parseErr := strconv.ParseInt(tag, 10, 64)
	if err != nil || parseErr != nil || rev < 0 || strconv.FormatInt(rev, 10) != tag || !strings.HasPrefix(r.Header.Get("If-Match"), "\"") {
		problem(w, r, http.StatusPreconditionRequired, "binding_revision_required", "Read the current binding and send its revision in If-Match.")
		return
	}
	var in struct {
		GrantID string `json:"grant_id"`
	}
	if r.Method == http.MethodPut {
		if !parse(w, r, &in, false) {
			return
		}
		if in.GrantID == "" {
			bad(w, r, "Select a grant to bind.")
			return
		}
	}
	result, err := s.GrantGateway.SetBinding(r.Context(), workspaceID(r), in.GrantID, rev)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			grantConflict(w, r, "binding_conflict", "The binding or target grant changed. Reload the binding and try again.", map[string]any{"binding_revision": result.BindingRevision})
			return
		}
		grantFailure(w, r, err)
		return
	}

	s.grantBinding(w, r)
}

func (s *Server) rotateGatewayCredential(w http.ResponseWriter, r *http.Request) {
	if err := s.GrantGateway.RotateCredential(r.Context(), workspaceID(r)); err != nil {
		grantFailure(w, r, err)
		return
	}
	s.prepareGatewayCredential(r)
	s.grantBinding(w, r)
}

func (s *Server) prepareGatewayCredential(r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	w, err := s.Store.Workspace(ctx, workspaceID(r))
	if err == nil {
		if err := s.Control.ReconcileGateway(ctx, w); err != nil {
			slog.Info("workspace gateway credential pending", "workspace_id", w.ID)
		}
	}
}

func grantConflict(w http.ResponseWriter, r *http.Request, code, message string, details map[string]any) {
	respond(w, r, http.StatusConflict, map[string]any{
		"error":      map[string]any{"code": code, "message": message, "details": details},
		"request_id": middleware.GetReqID(r.Context()),
	})
}

func grantFailure(w http.ResponseWriter, r *http.Request, err error) {
	var gateway *codexgateway.Error
	var upstream *codexupstream.UpstreamError
	switch {
	case errors.As(err, &gateway):
		problem(w, r, gateway.Status, gateway.Code, gateway.Message)
	case errors.As(err, &upstream):
		code := upstream.Code
		if code == "" {
			code = upstream.Stage + "_unavailable"
		}
		status := upstream.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		problem(w, r, status, code, "The selected grant could not complete the "+upstream.Stage+" step.")
	case errors.Is(err, store.ErrGrantUnavailable):
		problem(w, r, http.StatusConflict, "grant_unavailable", "The selected grant is unavailable.")
	default:
		failure(w, r, err)
	}
}
