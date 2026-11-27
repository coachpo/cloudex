package codexgateway

import (
	"context"
	"crypto/cipher"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/sync/singleflight"
)

type Config struct {
	Store       *store.Store
	Key         cipher.AEAD
	Origin      string
	RedirectURI string
	Clients     []*codexupstream.Client
}

type Error struct {
	Status               int
	Code, Param, Message string
}

func (e *Error) Error() string { return e.Message }

var (
	errCredential = &Error{Status: http.StatusUnauthorized, Code: "invalid_gateway_credential", Message: "The gateway credential is no longer active."}
	errNotReady   = &Error{Status: http.StatusServiceUnavailable, Code: "instance_not_verified", Message: "The Workspace container is waiting for gateway credential verification."}
	errGrantMoved = &Error{Status: http.StatusConflict, Code: "authorization_changed", Message: "The grant changed while the request was in progress."}
	errTokenMoved = &Error{Status: http.StatusConflict, Code: "token_version_changed", Message: "The access token was refreshed before the request could be admitted."}
	errDisabled   = &Error{Status: http.StatusServiceUnavailable, Code: "gateway_not_configured", Message: "Configure the HTTPS gateway origin on the control plane."}
)

type grantState struct {
	fence sync.RWMutex
	mu    sync.Mutex
	route routeSnapshot
	model modelSnapshot
}

type workspaceState struct {
	fence        sync.RWMutex
	prepare      sync.Mutex
	confirmed    store.GatewayCredential
	verification uint64
}

type Service struct {
	Store    *store.Store
	key      cipher.AEAD
	origin   string
	redirect string
	clients  map[string]*codexupstream.Client
	ctx      context.Context
	cancel   context.CancelFunc

	mu         sync.Mutex
	grants     map[string]*grantState
	workspaces map[string]*workspaceState
	active     map[*Admission]context.CancelFunc
	attempts   map[string]*authorizationAttempt
	states     map[string]string
	refreshes  singleflight.Group
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Key == nil {
		return nil, errors.New("gateway requires a database and encryption key")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Service{
		Store: cfg.Store, key: cfg.Key, origin: cfg.Origin, redirect: cfg.RedirectURI, ctx: ctx, cancel: cancel,
		clients: make(map[string]*codexupstream.Client), grants: make(map[string]*grantState),
		workspaces: make(map[string]*workspaceState), active: make(map[*Admission]context.CancelFunc),
		attempts: make(map[string]*authorizationAttempt), states: make(map[string]string),
	}
	for _, client := range cfg.Clients {
		s.clients[client.Channel()] = client
	}
	if err := s.Store.RecoverGrantRefreshes(ctx); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() {
	s.cancel()
	s.cancelAdmissions("", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.attempts)
	clear(s.states)
}

func (s *Service) grantState(id string) *grantState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grants[id] == nil {
		s.grants[id] = &grantState{}
	}
	return s.grants[id]
}

func (s *Service) workspaceState(id string) *workspaceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspaces[id] == nil {
		s.workspaces[id] = &workspaceState{}
	}
	return s.workspaces[id]
}

type Admission struct {
	Credential store.GatewayCredential
	Grant      store.Grant
	Context    context.Context

	service   *Service
	workspace *workspaceState
	grant     *grantState
}

func (s *Service) lookupCredential(ctx context.Context, digest string) (store.GatewayCredential, error) {
	c, err := s.Store.AuthenticateCredential(ctx, digest)
	if errors.Is(err, store.ErrGrantUnavailable) {
		g, err := s.Store.Grant(ctx, c.GrantID)
		if err != nil {
			return c, err
		}
		return c, checkGrant(g)
	}
	if errors.Is(err, store.ErrMissing) {
		return c, errCredential
	}
	return c, err
}

func (s *Service) Admit(ctx context.Context, bearer string) (*Admission, error) {
	if bearer == "" {
		return nil, errCredential
	}
	digest := store.GatewayCredentialDigest(bearer)
	initial, err := s.lookupCredential(ctx, digest)
	if err != nil {
		return nil, err
	}
	gs, ws := s.grantState(initial.GrantID), s.workspaceState(initial.WorkspaceID)
	gs.fence.RLock()
	defer gs.fence.RUnlock()
	ws.fence.RLock()
	defer ws.fence.RUnlock()
	if err := s.ctx.Err(); err != nil {
		return nil, errNotReady
	}
	c, err := s.lookupCredential(ctx, digest)
	if err != nil {
		return nil, err
	}
	if c != initial {
		return nil, errCredential
	}
	if ws.confirmed != c {
		return nil, errNotReady
	}
	g, err := s.Store.Grant(ctx, c.GrantID)
	if err != nil {
		return nil, err
	}
	if err := checkGrant(g); err != nil {
		return nil, err
	}
	run, cancel := context.WithCancel(ctx)
	// No transport handler needs the encrypted token group.
	g.Credential = nil
	a := &Admission{Credential: c, Grant: g, Context: run, service: s, workspace: ws, grant: gs}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		cancel()
		return nil, errNotReady
	}
	s.active[a] = cancel
	s.mu.Unlock()
	return a, nil
}

func (a *Admission) Release() {
	s := a.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.active[a]; ok {
		delete(s.active, a)
		cancel()
	}
}

// WithWrite keeps the binding and grant valid through the transport handoff.
func (a *Admission) WithWrite(tokenVersion int64, write func() error) error {
	a.grant.fence.RLock()
	defer a.grant.fence.RUnlock()
	a.workspace.fence.RLock()
	defer a.workspace.fence.RUnlock()
	if err := a.Context.Err(); err != nil {
		return err
	}
	c, err := a.service.lookupCredential(a.Context, a.Credential.Digest)
	if err != nil {
		return err
	}
	if c != a.Credential {
		return errCredential
	}
	if a.workspace.confirmed != c {
		return errNotReady
	}
	if tokenVersion != 0 {
		g, err := a.service.Store.Grant(a.Context, c.GrantID)
		if err != nil {
			return err
		}
		if g.TokenVersion != tokenVersion {
			return errTokenMoved
		}
	}
	return write()
}

func (s *Service) cancelAdmissions(workspaceID, grantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for a, cancel := range s.active {
		if (workspaceID == "" || a.Credential.WorkspaceID == workspaceID) && (grantID == "" || a.Grant.ID == grantID) {
			cancel()
			count++
		}
	}
	if count > 0 {
		slog.Info("gateway admissions canceled", "workspace_id", workspaceID, "grant_id", grantID,
			"count", count, "upstream_outcome", "requests already sent may still run")
	}
}

type Upstream struct {
	Client       *codexupstream.Client
	Token        codexupstream.Grant
	Route        codexupstream.Route
	TokenVersion int64
}

type routeSnapshot struct {
	epoch, version int64
	route          codexupstream.Route
}
type modelSnapshot struct {
	epoch   int64
	route   codexupstream.Route
	expires time.Time
	catalog codexupstream.Catalog
}

func (s *Service) upstream(ctx context.Context, grantID string, epoch int64) (Upstream, error) {
	g, token, err := s.currentToken(ctx, grantID, epoch)
	if err != nil {
		return Upstream{}, err
	}
	client := s.clients[g.Channel]
	if client == nil {
		return Upstream{}, errDisabled
	}
	state := s.grantState(grantID)
	state.mu.Lock()
	route := state.route
	state.mu.Unlock()
	if route.epoch != g.GrantEpoch || route.version != g.TokenVersion {
		value, err := client.ResolveRoute(ctx, token)
		if err != nil {
			return Upstream{}, err
		}
		route = routeSnapshot{epoch: g.GrantEpoch, version: g.TokenVersion, route: value}
		state.mu.Lock()
		if route.epoch > state.route.epoch || (route.epoch == state.route.epoch && route.version >= state.route.version) {
			state.route = route
		}
		state.mu.Unlock()
	}
	return Upstream{Client: client, Token: token, Route: route.route, TokenVersion: g.TokenVersion}, nil
}

func (s *Service) Upstream(ctx context.Context, a *Admission) (Upstream, error) {
	return s.upstream(ctx, a.Grant.ID, a.Grant.GrantEpoch)
}

func (s *Service) models(ctx context.Context, id string, epoch int64, upstream Upstream) (codexupstream.Catalog, error) {
	state := s.grantState(id)
	state.mu.Lock()
	cache := state.model
	state.mu.Unlock()
	if cache.epoch == epoch && cache.route == upstream.Route && time.Now().Before(cache.expires) {
		return cache.catalog, nil
	}
	catalog, err := upstream.Client.ListModels(ctx, upstream.Token, upstream.Route)
	if err != nil {
		return codexupstream.Catalog{}, err
	}
	if len(catalog.Slugs) == 0 {
		return codexupstream.Catalog{}, &Error{Status: http.StatusUnprocessableEntity, Code: "no_compatible_models", Message: "The grant's directory has no model compatible with Codex " + codexupstream.CodexClientVersion + "."}
	}
	state.mu.Lock()
	if epoch >= state.model.epoch {
		state.model = modelSnapshot{epoch: epoch, route: upstream.Route, catalog: catalog, expires: time.Now().Add(5 * time.Minute)}
	}
	state.mu.Unlock()
	return catalog, nil
}

func (s *Service) Models(ctx context.Context, a *Admission, upstream Upstream) (codexupstream.Catalog, error) {
	return s.models(ctx, a.Grant.ID, a.Grant.GrantEpoch, upstream)
}
