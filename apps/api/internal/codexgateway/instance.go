package codexgateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type BindingView struct {
	store.Binding
	Readiness string `json:"readiness"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Service) Binding(ctx context.Context, id string) (BindingView, error) {
	b, err := s.Store.Binding(ctx, id)
	if err != nil {
		return BindingView{}, err
	}
	view := BindingView{Binding: b, Readiness: "unbound"}
	if b.GrantID == "" {
		return view, nil
	}
	g, err := s.Store.Grant(ctx, b.GrantID)
	if err != nil {
		return view, err
	}
	if err := checkGrant(g); err != nil {
		view.Readiness, view.Reason = "unavailable", g.Reason
		return view, nil
	}
	view.Readiness = "pending"
	if s.origin == "" {
		view.Reason = errDisabled.Code
		return view, nil
	}
	if s.localOnly {
		w, err := s.Store.Workspace(ctx, id)
		if err != nil {
			return view, err
		}
		if !s.gatewayAvailableOn(w.HostID) {
			view.Reason = errDisabled.Code
			return view, nil
		}
	}
	ws := s.workspaceState(id)
	ws.fence.RLock()
	defer ws.fence.RUnlock()
	confirmed := ws.confirmed
	if confirmed.Status == store.CredentialActive && confirmed.GrantEpoch == g.GrantEpoch &&
		confirmed.BindingRevision == b.BindingRevision && confirmed.GrantID == b.GrantID {
		if current, err := s.Store.AuthenticateCredential(ctx, confirmed.Digest); err == nil && current == confirmed {
			view.Readiness = "ready"
		}
	}
	return view, nil
}

type bindingPreflight struct {
	epoch, tokenVersion int64
}

func (s *Service) gatewayAvailableOn(host string) bool {
	return s.origin != "" && (!s.localOnly || host == store.LocalHostID)
}

func (s *Service) preflight(ctx context.Context, id string, host string) (bindingPreflight, error) {
	if !s.gatewayAvailableOn(host) {
		return bindingPreflight{}, errDisabled
	}
	g, err := s.Store.Grant(ctx, id)
	if err != nil {
		return bindingPreflight{}, err
	}
	up, err := s.upstream(ctx, id, g.GrantEpoch)
	if err != nil {
		return bindingPreflight{}, err
	}
	_, err = s.models(ctx, id, g.GrantEpoch, up)
	return bindingPreflight{epoch: g.GrantEpoch, tokenVersion: up.TokenVersion}, err
}

func (s *Service) checkPreflight(ctx context.Context, id string, expected bindingPreflight) error {
	g, err := s.Store.Grant(ctx, id)
	if err != nil {
		return err
	}
	if err := checkGrant(g); err != nil {
		return err
	}
	if g.GrantEpoch != expected.epoch {
		return errGrantMoved
	}
	if g.TokenVersion != expected.tokenVersion {
		return errTokenMoved
	}
	return nil
}

func (s *Service) SetBinding(ctx context.Context, workspaceID, grantID string, expectedRevision int64) (store.Binding, error) {
	b, err := s.Store.Binding(ctx, workspaceID)
	if err != nil {
		return b, err
	}
	if b.BindingRevision != expectedRevision && b.GrantID != grantID {
		return b, store.ErrConflict
	}
	var checked bindingPreflight
	if grantID != "" && grantID != b.GrantID {
		var host string
		if s.localOnly {
			w, err := s.Store.Workspace(ctx, workspaceID)
			if err != nil {
				return b, err
			}
			host = w.HostID
		}
		checked, err = s.preflight(ctx, grantID, host)
		if err != nil {
			return b, err
		}
		fence := &s.grantState(grantID).fence
		fence.RLock()
		defer fence.RUnlock()
	}
	ws := s.workspaceState(workspaceID)
	ws.fence.Lock()
	defer ws.fence.Unlock()
	current, err := s.Store.Binding(ctx, workspaceID)
	if err != nil || current.GrantID == grantID {
		return current, err
	}
	if current.BindingRevision != expectedRevision || (grantID != "" && checked.epoch == 0) {
		return current, store.ErrConflict
	}
	if grantID != "" {
		if err := s.checkPreflight(ctx, grantID, checked); err != nil {
			return current, err
		}
	}
	result, err := s.Store.SetBinding(ctx, workspaceID, grantID, expectedRevision, checked.epoch)
	if err == nil {
		s.invalidateWorkspace(ws, workspaceID)
	}
	return result, err
}

func (s *Service) CreateWorkspace(ctx context.Context, w store.Workspace, key, hash string, repo *store.RepositoryCredential, grantID string) (store.Operation, error) {
	if op, err := s.Store.IdempotentOperation(ctx, key, hash); !errors.Is(err, store.ErrMissing) {
		return op, err
	}
	host := w.HostID
	if host == "" {
		host = store.LocalHostID
	}
	checked, err := s.preflight(ctx, grantID, host)
	if err != nil {
		if op, replayErr := s.Store.IdempotentOperation(ctx, key, hash); !errors.Is(replayErr, store.ErrMissing) {
			return op, replayErr
		}
		return store.Operation{}, err
	}
	fence := &s.grantState(grantID).fence
	fence.RLock()
	defer fence.RUnlock()
	if err := s.checkPreflight(ctx, grantID, checked); err != nil {
		return store.Operation{}, err
	}
	return s.Store.CreateWithGrant(ctx, w, key, hash, repo, grantID, checked.epoch)
}

func (s *Service) RotateCredential(ctx context.Context, id string) error {
	ws := s.workspaceState(id)
	ws.fence.Lock()
	defer ws.fence.Unlock()
	binding, err := s.Store.Binding(ctx, id)
	if err != nil {
		return err
	}
	if binding.GrantID == "" {
		return &Error{Status: http.StatusConflict, Code: "workspace_unbound", Message: "Bind a grant to the Workspace before rotating its gateway credential."}
	}
	if err := s.Store.RevokeWorkspaceCredential(ctx, id); err != nil {
		return err
	}
	s.invalidateWorkspace(ws, id)
	return nil
}

// Enqueue uses the same fence as transport writes. The operation and
// credential revocation commit together in the store.
func (s *Service) Enqueue(ctx context.Context, id, kind string, purge bool, key, hash string) (store.Operation, error) {
	ws := s.workspaceState(id)
	ws.fence.Lock()
	defer ws.fence.Unlock()
	op, err := s.Store.Enqueue(ctx, id, kind, purge, key, hash)
	if err != nil {
		return op, err
	}
	if op.Status == "queued" || op.Status == "running" {
		s.invalidateWorkspace(ws, id)
	}
	return op, nil
}

func (s *Service) invalidateWorkspace(ws *workspaceState, id string) {
	ws.confirmed = store.GatewayCredential{}
	ws.verification++
	s.cancelAdmissions(id, "")
}

func (s *Service) SuspendGateway(_ context.Context, id string) error {
	ws := s.workspaceState(id)
	ws.fence.Lock()
	defer ws.fence.Unlock()
	s.invalidateWorkspace(ws, id)
	return nil
}

func (s *Service) ReconcileGateway(ctx context.Context, w store.Workspace, info runtime.Container, host runtime.GatewayCredentialRuntime) error {
	if !s.gatewayAvailableOn(w.HostID) {
		return nil
	}
	if info.State != "running" || info.ID == "" || info.StartedAt == "" {
		return s.SuspendGateway(ctx, w.ID)
	}
	ws := s.workspaceState(w.ID)
	ws.prepare.Lock()
	defer ws.prepare.Unlock()
	ws.fence.RLock()
	verification := ws.verification
	b, err := s.Store.Binding(ctx, w.ID)
	ws.fence.RUnlock()
	if err != nil || b.GrantID == "" {
		return err
	}
	g, err := s.Store.Grant(ctx, b.GrantID)
	if err != nil || g.Status != store.GrantActive {
		return err
	}
	cred, err := s.Store.WorkspaceCredential(ctx, w.ID)
	if err != nil && !errors.Is(err, store.ErrMissing) {
		return err
	}
	spec := control.WorkspaceSpec(w)
	instance := runtime.GatewayInstance{ContainerID: info.ID, StartedAt: info.StartedAt}
	confirmation, err := host.InspectGatewayCredential(ctx, spec, instance)
	if err != nil {
		ws.fence.Lock()
		if !errors.Is(err, runtime.ErrOwnership) || ws.confirmed.StartedAt == info.StartedAt {
			s.invalidateWorkspace(ws, w.ID)
		}
		ws.fence.Unlock()
		return err
	}
	if (cred.Status == store.CredentialActive || cred.Status == store.CredentialPending) &&
		cred.WorkspaceID == w.ID && cred.HostID == w.HostID && cred.Generation == w.Generation &&
		cred.DockerID == instance.ContainerID && cred.StartedAt == instance.StartedAt && cred.GrantID == g.ID &&
		cred.BindingRevision == b.BindingRevision && cred.GrantEpoch == g.GrantEpoch && s.matchesConfirmation(cred, confirmation) {
		return s.confirmInstallation(ctx, ws, verification, cred)
	}

	token := opaqueSecret()
	seed := store.GatewayCredential{WorkspaceID: w.ID, HostID: w.HostID, Generation: w.Generation,
		DockerID: instance.ContainerID, StartedAt: instance.StartedAt, GrantID: g.ID,
		BindingRevision: b.BindingRevision, GrantEpoch: g.GrantEpoch, Digest: store.GatewayCredentialDigest(token)}
	ws.fence.Lock()
	if ws.verification != verification {
		ws.fence.Unlock()
		return store.ErrBusy
	}
	pending, err := s.Store.IssueCredential(ctx, seed)
	if err != nil {
		ws.fence.Unlock()
		return err
	}
	s.invalidateWorkspace(ws, w.ID)
	verification = ws.verification
	ws.fence.Unlock()

	confirmation, err = host.InstallGatewayCredential(ctx, spec, instance, runtime.GatewayCredentialInstall{
		Token: token, Origin: s.origin,
		BindingRevision: pending.BindingRevision, GrantID: pending.GrantID, GrantEpoch: pending.GrantEpoch, Sequence: pending.Sequence,
	})
	if err != nil {
		return err
	}
	if !s.matchesConfirmation(pending, confirmation) {
		return errNotReady
	}
	return s.confirmInstallation(ctx, ws, verification, pending)
}

func (s *Service) matchesConfirmation(cred store.GatewayCredential, confirmation runtime.GatewayCredentialConfirmation) bool {
	return cred.Digest == confirmation.Digest && cred.Sequence == confirmation.Sequence && cred.StartedAt == confirmation.StartedAt &&
		cred.BindingRevision == confirmation.BindingRevision && cred.GrantID == confirmation.GrantID && cred.GrantEpoch == confirmation.GrantEpoch &&
		s.origin == confirmation.Origin
}

func (s *Service) confirmInstallation(ctx context.Context, ws *workspaceState, verification uint64, cred store.GatewayCredential) error {
	ws.fence.Lock()
	defer ws.fence.Unlock()
	if ws.verification != verification {
		return store.ErrBusy
	}
	if err := s.Store.ActivateCredential(ctx, cred); err != nil {
		return err
	}
	cred.Status = store.CredentialActive
	ws.confirmed = cred
	return nil
}
