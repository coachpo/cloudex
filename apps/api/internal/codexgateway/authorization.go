package codexgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const authorizationLifetime = 10 * time.Minute

type authorizationAttempt struct {
	id, owner, channel, state, verifier, nonce string
	redirectURI                                string
	previous                                   codexupstream.Grant
	grantID                                    string
	epoch                                      int64
	token                                      codexupstream.Grant
	status, failure, failureCode               string
	expires                                    time.Time
	cancelDevice                               context.CancelFunc
}

type AuthorizationStart struct {
	ID               string                                  `json:"id"`
	AuthorizationURL string                                  `json:"authorization_url,omitempty"`
	Device           *codexupstream.CodexDeviceAuthorization `json:"device,omitempty"`
}

type AuthorizationStatus struct {
	ID       string       `json:"id"`
	Status   string       `json:"status"`
	Error    string       `json:"error,omitempty"`
	Code     string       `json:"code,omitempty"`
	Identity *store.Grant `json:"identity,omitempty"`
}

func opaqueSecret() string {
	var raw [32]byte
	rand.Read(raw[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func (s *Service) BeginAuthorization(ctx context.Context, owner, channel, grantID string) (AuthorizationStart, error) {
	if s.origin == "" || s.redirect == "" {
		return AuthorizationStart{}, errDisabled
	}
	a, consent, err := s.prepareAuthorization(ctx, owner, channel, grantID)
	if err != nil {
		return AuthorizationStart{}, err
	}
	a.state, a.nonce, a.verifier = opaqueSecret(), opaqueSecret(), opaqueSecret()
	a.redirectURI = s.redirect
	a.expires = time.Now().Add(authorizationLifetime)
	var hostID string
	if channel == codexupstream.ChannelChatGPT {
		hostID, err = s.Store.AgentHostID(ctx)
		if err != nil {
			return AuthorizationStart{}, err
		}
	}
	challenge := sha256.Sum256([]byte(a.verifier))
	url := s.clients[channel].AuthorizeURL(codexupstream.Authorization{State: a.state, Challenge: base64.RawURLEncoding.EncodeToString(challenge[:]),
		Redirect: a.redirectURI, Nonce: a.nonce, AgentHostID: hostID, Previous: a.previous, Consent: consent})
	if err := s.trackAuthorization(a); err != nil {
		return AuthorizationStart{}, err
	}
	return AuthorizationStart{ID: a.id, AuthorizationURL: url}, nil
}

func (s *Service) prepareAuthorization(ctx context.Context, owner, channel, grantID string) (*authorizationAttempt, bool, error) {
	if s.clients[channel] == nil {
		return nil, false, &Error{Status: http.StatusBadRequest, Code: "unknown_channel", Message: "Select a supported sign-in channel."}
	}
	a := &authorizationAttempt{id: store.ID(), owner: owner, channel: channel, grantID: grantID, status: "pending"}
	var consent bool
	if grantID != "" {
		g, err := s.Store.Grant(ctx, grantID)
		if err != nil {
			return nil, false, err
		}
		if channel != g.Channel {
			return nil, false, &Error{Status: http.StatusConflict, Code: "channel_mismatch", Message: "Use the original grant's sign-in channel."}
		}
		a.previous = codexupstream.Grant{ClientID: g.ClientID, Subject: g.Subject, UserID: g.UpstreamUserID, AccountID: g.UpstreamWorkspaceID}
		a.epoch = g.GrantEpoch
		consent = g.Status == store.GrantMissingScope
	}
	return a, consent, nil
}

func (s *Service) trackAuthorization(a *authorizationAttempt) error {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return errNotReady
	}
	s.attempts[a.id] = a
	if a.state != "" {
		s.states[a.state] = a.id
	}
	s.mu.Unlock()
	id, state := a.id, a.state
	time.AfterFunc(time.Until(a.expires), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.attempts, id)
		delete(s.states, state)
	})
	return nil
}

var (
	errInvalidState = &Error{Status: http.StatusBadRequest, Code: "invalid_oauth_state", Message: "Sign-in expired or the callback was already used."}
	errWrongAccount = &Error{Status: http.StatusConflict, Code: "oauth_identity_mismatch", Message: "The selected account, workspace or registration differs from this grant."}
)

func authorizationExchangeError(err error) error {
	var upstream *codexupstream.UpstreamError
	if errors.As(err, &upstream) {
		switch {
		case upstream.Stage == "device_poll":
			return &Error{Status: http.StatusBadRequest, Code: "device_code_authorization_failed", Message: "OpenAI did not complete device sign-in. Start a new attempt."}
		case upstream.Code == "identity_mismatch":
			return errWrongAccount
		case upstream.Code == "codex_workspace_unverified":
			return &Error{Status: http.StatusBadRequest, Code: "codex_workspace_unverified", Message: "The Codex authorization did not verify an upstream workspace."}
		case upstream.Stage == "identity":
			return &Error{Status: http.StatusBadRequest, Code: "oauth_identity_unverified", Message: "The provider's identity could not be verified. Check the account and workspace before trying again."}
		}
	}
	return &Error{Status: http.StatusBadRequest, Code: "oauth_exchange_failed", Message: "Sign-in failed at the provider's token exchange."}
}

func (s *Service) finishAuthorization(a *authorizationAttempt, token codexupstream.Grant, err error) error {
	if err == nil && a.grantID != "" &&
		(token.ClientID != a.previous.ClientID || token.Subject != a.previous.Subject ||
			token.UserID != a.previous.UserID || token.AccountID != a.previous.AccountID) {
		err = errWrongAccount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts[a.id] != a || !time.Now().Before(a.expires) {
		return errInvalidState
	}
	a.verifier, a.nonce = "", ""
	if err != nil {
		a.status, a.failure = "failed", err.Error()
		var failure *Error
		if errors.As(err, &failure) {
			a.failureCode = failure.Code
		}
	} else {
		a.status, a.token = "ready", token
	}
	return err
}

func (s *Service) AuthorizationStatus(owner, id string) (AuthorizationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[id]
	if a == nil || a.owner != owner || !time.Now().Before(a.expires) {
		return AuthorizationStatus{}, store.ErrMissing
	}
	result := AuthorizationStatus{ID: a.id, Status: a.status, Error: a.failure, Code: a.failureCode}
	if a.status == "ready" {
		result.Identity = &store.Grant{Channel: a.channel, ClientID: a.token.ClientID, Subject: a.token.Subject,
			UpstreamUserID: a.token.UserID, UpstreamWorkspaceID: a.token.AccountID, DisplayName: a.token.DisplayName, Email: a.token.Email}
	}
	return result, nil
}

func (s *Service) ConfirmAuthorization(ctx context.Context, owner, id string) (store.Grant, error) {
	s.mu.Lock()
	a := s.attempts[id]
	if a == nil || a.owner != owner || !time.Now().Before(a.expires) {
		s.mu.Unlock()
		return store.Grant{}, store.ErrMissing
	}
	if a.status != "ready" {
		s.mu.Unlock()
		return store.Grant{}, &Error{Status: http.StatusConflict, Code: "authorization_not_ready", Message: "Sign-in must complete before the grant can be saved."}
	}
	delete(s.attempts, id)
	s.mu.Unlock()
	grantID := a.grantID
	if grantID == "" {
		grantID = store.ID()
	}
	g, err := s.grantRecord(grantID, s.clients[a.channel], a.token)
	if err != nil {
		return g, err
	}
	if a.grantID == "" {
		err = s.Store.AddGrant(ctx, g)
		if errors.Is(err, store.ErrConflict) {
			return g, &Error{Status: http.StatusConflict, Code: "grant_exists", Message: "This identity already has a grant for this registration and workspace."}
		}
	} else {
		fence := &s.grantState(g.ID).fence
		fence.Lock()
		err = s.Store.ReplaceGrant(ctx, g, a.epoch)
		if err == nil {
			s.cancelAdmissions("", g.ID)
		}
		fence.Unlock()
		if errors.Is(err, store.ErrConflict) {
			return g, errGrantMoved
		}
	}
	if err != nil {
		return g, err
	}
	return s.Store.Grant(ctx, g.ID)
}

func (s *Service) DisconnectGrant(ctx context.Context, id string) (store.Grant, error) {
	fence := &s.grantState(id).fence
	fence.Lock()
	g, err := s.Store.Grant(ctx, id)
	if err != nil || g.Status == store.GrantDisconnected {
		fence.Unlock()
		return g, err
	}
	token, openErr := s.openGrant(g)
	epoch, err := s.Store.DisconnectGrant(ctx, id, g.GrantEpoch)
	if err != nil {
		fence.Unlock()
		return g, err
	}
	s.cancelAdmissions("", id)
	fence.Unlock()

	reason := "remote_revocation_unconfirmed"
	if openErr == nil {
		work, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		if err := s.clients[g.Channel].Revoke(work, token); err == nil && g.RefreshAttemptID == "" {
			reason = ""
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.FinishGrantRevocation(save, id, epoch, reason); err != nil && !errors.Is(err, store.ErrConflict) {
		return g, err
	}
	return s.Store.Grant(save, id)
}

func (s *Service) DeleteGrant(ctx context.Context, id string) error {
	fence := &s.grantState(id).fence
	fence.Lock()
	defer fence.Unlock()
	return s.Store.DeleteGrant(ctx, id)
}
