package codexgateway

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type grantTestHost struct {
	active  runtime.GatewayCredentialInstall
	current runtime.GatewayInstance
	loseAck bool
}

func (h *grantTestHost) InspectGatewayCredential(_ context.Context, _ runtime.Spec, instance runtime.GatewayInstance) (runtime.GatewayCredentialConfirmation, error) {
	if instance != h.current {
		return runtime.GatewayCredentialConfirmation{}, runtime.ErrOwnership
	}
	return runtime.GatewayCredentialConfirmation{
		StartedAt: instance.StartedAt, Digest: store.GatewayCredentialDigest(h.active.Token), Sequence: h.active.Sequence,
		Origin: h.active.Origin, BindingRevision: h.active.BindingRevision, GrantID: h.active.GrantID, GrantEpoch: h.active.GrantEpoch,
	}, nil
}

func (h *grantTestHost) InstallGatewayCredential(ctx context.Context, spec runtime.Spec, instance runtime.GatewayInstance, install runtime.GatewayCredentialInstall) (runtime.GatewayCredentialConfirmation, error) {
	if instance != h.current {
		return runtime.GatewayCredentialConfirmation{}, runtime.ErrOwnership
	}
	h.active = install
	if h.loseAck {
		h.loseAck = false
		return runtime.GatewayCredentialConfirmation{}, runtime.ErrHostUnavailable
	}
	return h.InspectGatewayCredential(ctx, spec, instance)
}

func testGrantGateway(t *testing.T, upstream string) (Config, *Service) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	config := codexupstream.ChatGPTConfig()
	if upstream != "" {
		config.Issuer, config.TokenURL, config.DiscoveryURL = upstream, upstream+"/token", upstream+"/discovery"
	}
	client, err := codexupstream.New(config)
	if err != nil {
		t.Fatal(err)
	}
	codexConfig := codexupstream.OpenAICodexConfig()
	if upstream != "" {
		codexConfig.Issuer, codexConfig.AccountsURL = upstream, upstream+"/accounts"
	}
	codex, err := codexupstream.New(codexConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: db, Key: key, Origin: "https://gateway.example.test", Clients: []*codexupstream.Client{client, codex}}
	s, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return cfg, s
}

func seedTestGrant(t *testing.T, s *Service, identity string, expires time.Time) store.Grant {
	t.Helper()
	return persistTestGrant(t, s, codexupstream.ChannelChatGPT, codexupstream.Grant{
		ClientID: "registration-" + identity, Subject: identity, Resource: codexupstream.ChatGPTResource,
		Scopes: []string{"offline_access", "resource.invoke", "chatgpt.tokens.use.direct"}, ExpiresAt: expires,
		AccessToken: "earlier-" + identity, RefreshToken: "renewable-" + identity,
	})
}

func persistTestGrant(t *testing.T, s *Service, channel string, token codexupstream.Grant) store.Grant {
	t.Helper()
	g, err := s.grantRecord(store.ID(), s.clients[channel], token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddGrant(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	g, err = s.Store.Grant(t.Context(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func runningWithTestGrant(t *testing.T, s *Service, grant store.Grant, name string) (store.Workspace, runtime.Container, *grantTestHost) {
	t.Helper()
	ctx := t.Context()
	w := store.Workspace{ID: store.ID(), Name: name, Image: "fixture", Branch: "main", Desired: "running", CPU: 1000, Memory: 1024}
	queued, err := s.Store.CreateWithGrant(ctx, w, "", "", nil, grant.ID, grant.GrantEpoch)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Store.ClaimOp(ctx)
	if err != nil || op.ID != queued.ID {
		t.Fatalf("claim create: %v", err)
	}
	info := runtime.Container{ID: store.ID(), ImageID: "fixture", State: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	observed := store.Container{Generation: op.Target, ID: info.ID, ImageID: info.ImageID, Status: info.State}
	if err := s.Store.FinishOp(ctx, op, info.State, &observed, ""); err != nil {
		t.Fatal(err)
	}
	w, err = s.Store.Workspace(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return w, info, &grantTestHost{current: runtime.GatewayInstance{ContainerID: info.ID, StartedAt: info.StartedAt}}
}

func installedTestAdmission(t *testing.T, s *Service, grant store.Grant, name string) *Admission {
	t.Helper()
	w, info, host := runningWithTestGrant(t, s, grant, name)
	if err := s.ReconcileGateway(t.Context(), w, info, host); err != nil {
		t.Fatal(err)
	}
	a, err := s.Admit(t.Context(), host.active.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Release)
	return a
}

func TestGatewaySharedGrantRefresh(t *testing.T) {
	var count atomic.Int32
	seen := make(chan []string, 1)
	resume := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/accounts" {
			if r.Header.Get("Authorization") != "Bearer codex-b" || r.Header.Get("ChatGPT-Account-ID") != "workspace-b" {
				t.Error("Codex route lookup used the wrong synthetic identity")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{{
				"id": "workspace-b", "workspace_backend_origin": "NO_CONSTRAINT", "account_routing_override": "NO_CONSTRAINT",
			}}})
			return
		}
		exchange := count.Add(1)
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		if exchange == 1 {
			seen <- []string{r.URL.Path, r.Form.Get("client_id"), r.Form.Get("refresh_token"), r.Form.Get("resource")}
		}
		select {
		case <-resume:
		case <-r.Context().Done():
			return
		}
		writeRotatedGrant(w, r.Host, "a")
	}))
	t.Cleanup(upstream.Close)
	_, s := testGrantGateway(t, upstream.URL)
	a := seedTestGrant(t, s, "a", time.Now().Add(-time.Minute))
	w1 := installedTestAdmission(t, s, a, "shared-one")
	w2 := installedTestAdmission(t, s, a, "shared-two")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		up  Upstream
		err error
	}
	done := make(chan result, 2)
	for _, admission := range []*Admission{w1, w2} {
		go func() {
			up, err := s.Upstream(ctx, admission)
			done <- result{up: up, err: err}
		}()
	}
	select {
	case got := <-seen:
		if !slices.Equal(got, []string{"/token", "registration-a", "renewable-a", codexupstream.ChatGPTResource}) {
			t.Fatal("refresh reached the wrong fake endpoint or test identity")
		}
	case <-ctx.Done():
		t.Fatal("shared requests did not trigger an exchange")
	}
	b := persistTestGrant(t, s, codexupstream.ChannelCodex, codexupstream.Grant{
		ClientID: codexupstream.CodexClientID, Subject: "b", UserID: "user-b", AccountID: "workspace-b",
		AccessToken: "codex-b", RefreshToken: "codex-renewable-b", ExpiresAt: time.Now().Add(time.Hour),
	})
	w3 := installedTestAdmission(t, s, b, "independent")
	if up, err := s.Upstream(ctx, w3); err != nil || up.Token.Subject != "b" || up.Client.Channel() != codexupstream.ChannelCodex {
		t.Fatalf("Codex grant B blocked during ChatGPT grant A refresh: %v", err)
	}
	release.Do(func() { close(resume) })
	for range 2 {
		out := <-done
		if out.err != nil || out.up.Token.Subject != "a" || out.up.Token.RefreshToken != "rotated-a" || out.up.TokenVersion != a.TokenVersion+1 {
			t.Fatalf("shared requests did not receive the same new version: %v", out.err)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("shared grant exchanged %d times", count.Load())
	}
	for _, admission := range []*Admission{w1, w2} {
		if err := admission.WithWrite(0, func() error { return nil }); err != nil {
			t.Fatalf("normal refresh revoked an existing admission: %v", err)
		}
		if err := admission.WithWrite(a.TokenVersion, func() error { return nil }); !errors.Is(err, errTokenMoved) {
			t.Fatalf("stale token version admitted a new handshake: %v", err)
		}
	}
	t.Log("W1 and W2 shared one refresh exchange for A, B remained available, and A's existing admissions survived")
}

func TestGatewayUnknownRefreshStopsOnlyItsGrant(t *testing.T) {
	var count atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		_ = r.ParseForm()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(upstream.Close)
	_, s := testGrantGateway(t, upstream.URL)
	a := seedTestGrant(t, s, "a", time.Now().Add(-time.Minute))
	w1 := installedTestAdmission(t, s, a, "a-one")
	w2 := installedTestAdmission(t, s, a, "a-two")
	b := seedTestGrant(t, s, "b", time.Now().Add(time.Hour))
	w3 := installedTestAdmission(t, s, b, "b-one")
	if _, err := s.Upstream(t.Context(), w1); err == nil {
		t.Fatal("accepted a missing refresh reply")
	}
	if w1.Context.Err() == nil || w2.Context.Err() == nil || w3.Context.Err() != nil {
		t.Fatal("refresh failure did not isolate affected admissions")
	}
	if _, _, err := s.currentToken(t.Context(), a.ID, a.GrantEpoch); err == nil || count.Load() != 1 {
		t.Fatalf("retried a possibly consumed refresh token: %d, %v", count.Load(), err)
	}
	g, err := s.Store.Grant(t.Context(), a.ID)
	if err != nil || g.Status != store.GrantReauthRequired || g.Reason != "refresh_outcome_unknown" || g.GrantEpoch != a.GrantEpoch+1 {
		t.Fatalf("unknown refresh not recorded: %s, %s, %v", g.Status, g.Reason, err)
	}
}

func TestGatewayInstallationRecoveryAndOriginChange(t *testing.T) {
	cfg, s := testGrantGateway(t, "")
	a := seedTestGrant(t, s, "a", time.Now().Add(time.Hour))
	w, info, host := runningWithTestGrant(t, s, a, "recovering")
	ctx := t.Context()
	host.loseAck = true
	if err := s.ReconcileGateway(ctx, w, info, host); !errors.Is(err, runtime.ErrHostUnavailable) {
		t.Fatalf("expected lost installation acknowledgement, got %v", err)
	}
	if host.active.GrantID != a.ID {
		t.Fatalf("installed grant ID = %q, want %q", host.active.GrantID, a.ID)
	}
	oldToken := host.active.Token
	if _, err := s.Admit(ctx, oldToken); err == nil {
		t.Fatal("admitted pending credential")
	}
	s.Close()
	restarted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.Admit(ctx, oldToken); err == nil {
		t.Fatal("admitted credential before checking Host")
	}
	if err := restarted.ReconcileGateway(ctx, w, info, host); err != nil || oldToken != host.active.Token {
		t.Fatalf("did not recover installed credential: %v", err)
	}
	admission, err := restarted.Admit(ctx, oldToken)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Release()
	if err := restarted.SuspendGateway(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if admission.Context.Err() == nil {
		t.Fatal("host suspension left an active admission")
	}
	if err := restarted.ReconcileGateway(ctx, w, info, host); err != nil || oldToken != host.active.Token {
		t.Fatalf("same instance failed to recover after offline: %v", err)
	}
	altered := cfg
	altered.Origin = "https://other-gateway.example.test"
	restarted.Close()
	moved, err := New(ctx, altered)
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	if err := moved.ReconcileGateway(ctx, w, info, host); err != nil || oldToken == host.active.Token || host.active.Origin != altered.Origin {
		t.Fatalf("retained old origin after control-plane restart: %v", err)
	}
	if _, err := moved.Admit(ctx, oldToken); !errors.Is(err, errCredential) {
		t.Fatalf("admitted credential for old gateway origin: %v", err)
	}
	stale := info
	stale.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if err := moved.ReconcileGateway(ctx, w, stale, host); !errors.Is(err, runtime.ErrOwnership) {
		t.Fatalf("accepted stale instance: %v", err)
	}
	live, err := moved.Admit(ctx, host.active.Token)
	if err != nil {
		t.Fatalf("stale instance affected the current credential: %v", err)
	}
	live.Release()
}

func writeRotatedGrant(w http.ResponseWriter, host, identity string) {
	claims, _ := json.Marshal(map[string]any{
		"sub": identity, "iss": "http://" + host, "aud": codexupstream.ChatGPTResource, "client_id": "registration-" + identity,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".c3ludGhldGlj",
		"refresh_token": "rotated-" + identity, "expires_in": 3600, "token_type": "bearer",
		"scope": "chatgpt.tokens.use.direct resource.invoke offline_access",
	})
}

func TestGatewayRecoveryAfterExchangeBeforePersistence(t *testing.T) {
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		writeRotatedGrant(w, r.Host, "a")
	}))
	t.Cleanup(upstream.Close)

	cfg, service := testGrantGateway(t, upstream.URL)
	ctx := t.Context()
	g := seedTestGrant(t, service, "a", time.Now().Add(-time.Minute))
	if err := service.Store.BeginGrantRefresh(ctx, g.ID, g.GrantEpoch, g.TokenVersion, store.ID()); err != nil {
		t.Fatal(err)
	}
	before, err := service.openGrant(g)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.clients[g.Channel].Refresh(ctx, before)
	if err != nil || result.RefreshToken != "rotated-a" {
		t.Fatalf("fake upstream did not rotate the grant: %v", err)
	}
	// Restart with the attempt durable and the successful exchange not yet saved.
	service.Close()
	recovered, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, _, err := recovered.currentToken(ctx, g.ID, g.GrantEpoch); err == nil {
		t.Fatal("the preceding grant was still usable after restart")
	}
	after, err := service.Store.Grant(ctx, g.ID)
	if err != nil || after.Reason != "refresh_outcome_unknown" ||
		after.GrantEpoch != g.GrantEpoch+1 || after.TokenVersion != g.TokenVersion || exchanges.Load() != 1 {
		t.Fatalf("restart did not fence the unresolved exchange: %v, requests=%d", err, exchanges.Load())
	}
}

func TestGatewayReauthorizationFencesLateRefresh(t *testing.T) {
	sent, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exchanges.Add(1) == 1 {
			close(sent)
		}
		select {
		case <-resume:
			writeRotatedGrant(w, r.Host, "a")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	_, s := testGrantGateway(t, upstream.URL)
	a := seedTestGrant(t, s, "a", time.Now().Add(-time.Minute))
	w1 := installedTestAdmission(t, s, a, "reauth-one")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Upstream(ctx, w1)
		done <- err
	}()
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("fake upstream did not receive the refresh")
	}

	newToken, err := s.openGrant(a)
	if err != nil {
		t.Fatal(err)
	}
	newToken.AccessToken, newToken.RefreshToken, newToken.ExpiresAt = "reauth-access", "reauth-refresh", time.Now().Add(time.Hour)
	// This fixture starts after the replacement identity has been verified.
	attempt := &authorizationAttempt{id: store.ID(), owner: "fixture", grantID: a.ID, channel: a.Channel, epoch: a.GrantEpoch,
		token: newToken, status: "ready", expires: time.Now().Add(time.Minute)}
	s.mu.Lock()
	s.attempts[attempt.id] = attempt
	s.mu.Unlock()
	g, err := s.ConfirmAuthorization(ctx, attempt.owner, attempt.id)
	if err != nil || g.GrantEpoch != a.GrantEpoch+1 || w1.Context.Err() == nil {
		t.Fatalf("replacement did not fence original admission: %v", err)
	}
	release.Do(func() { close(resume) })
	if err := <-done; !errors.Is(err, errGrantMoved) {
		t.Fatalf("late refresh returned %v, want grant epoch conflict", err)
	}
	_, loaded, err := s.currentToken(ctx, a.ID, g.GrantEpoch)
	if err != nil || loaded.AccessToken != newToken.AccessToken || loaded.RefreshToken != newToken.RefreshToken || exchanges.Load() != 1 {
		t.Fatalf("late refresh changed the replacement: %v", err)
	}
}

func TestGatewayDisconnectStopsOnlyAffectedGrant(t *testing.T) {
	var revocations atomic.Int32
	tokenSeen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": "http://" + r.Host, "revocation_endpoint": "http://" + r.Host + "/revoke",
			})
		case "/revoke":
			revocations.Add(1)
			_ = r.ParseForm()
			tokenSeen <- r.Form.Get("client_id") + ":" + r.Form.Get("token_type_hint") + ":" + r.Form.Get("token")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	_, s := testGrantGateway(t, upstream.URL)
	a := seedTestGrant(t, s, "a", time.Now().Add(time.Hour))
	w1 := installedTestAdmission(t, s, a, "disconnect-a-one")
	w2 := installedTestAdmission(t, s, a, "disconnect-a-two")
	b := seedTestGrant(t, s, "b", time.Now().Add(time.Hour))
	w3 := installedTestAdmission(t, s, b, "disconnect-b-one")
	if g, err := s.DisconnectGrant(t.Context(), a.ID); err != nil || g.Status != store.GrantDisconnected || g.Reason != "" || len(g.Credential) != 0 {
		t.Fatalf("disconnect did not clear the committed credentials: %v", err)
	}
	select {
	case seen := <-tokenSeen:
		if seen != "registration-a:refresh_token:renewable-a" {
			t.Fatal("remote revocation selected the wrong registration or token")
		}
	default:
		t.Fatal("fake upstream did not receive revocation")
	}
	if revocations.Load() != 1 || w1.Context.Err() == nil || w2.Context.Err() == nil || w3.Context.Err() != nil {
		t.Fatal("revocation affected the wrong scope")
	}
	if up, err := s.Upstream(t.Context(), w3); err != nil || up.Token.Subject != "b" {
		t.Fatalf("independent grant was unavailable after disconnect: %v", err)
	}
	if refs, err := s.Store.GrantWorkspaces(t.Context(), a.ID); err != nil || len(refs) != 2 {
		t.Fatalf("disconnect lost its affected-Workspace references: %v", err)
	}
}
