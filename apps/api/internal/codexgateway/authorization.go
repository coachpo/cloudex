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
	previous                                   codexupstream.Grant
	grantID                                    string
	epoch                                      int64
	token                                      codexupstream.Grant
	status, failure, failureCode               string
	expires                                    time.Time
}

type AuthorizationStart struct {
	ID               string `json:"id"`
	AuthorizationURL string `json:"authorization_url"`
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
	client := s.clients[channel]
	if client == nil {
		return AuthorizationStart{}, &Error{Status: http.StatusBadRequest, Code: "unknown_channel", Message: "Select a supported sign-in channel."}
	}
	a := &authorizationAttempt{id: store.ID(), state: opaqueSecret(), nonce: opaqueSecret(), verifier: opaqueSecret(),
		owner: owner, channel: channel, grantID: grantID, status: "pending", expires: time.Now().Add(authorizationLifetime)}
	var consent bool
	if grantID != "" {
		g, err := s.Store.Grant(ctx, grantID)
		if err != nil {
			return AuthorizationStart{}, err
		}
		if channel != g.Channel {
			return AuthorizationStart{}, &Error{Status: http.StatusConflict, Code: "channel_mismatch", Message: "Use the original grant's sign-in channel."}
		}
		a.previous = codexupstream.Grant{ClientID: g.ClientID, Subject: g.Subject, UserID: g.UpstreamUserID, AccountID: g.UpstreamWorkspaceID}
		a.epoch = g.GrantEpoch
		consent = g.Status == store.GrantMissingScope
	}
	var hostID string
	if channel == codexupstream.ChannelChatGPT {
		var err error
		hostID, err = s.Store.AgentHostID(ctx)
		if err != nil {
			return AuthorizationStart{}, err
		}
	}
	challenge := sha256.Sum256([]byte(a.verifier))
	url := client.AuthorizeURL(codexupstream.Authorization{State: a.state, Challenge: base64.RawURLEncoding.EncodeToString(challenge[:]),
		Redirect: s.redirect, Nonce: a.nonce, AgentHostID: hostID, Previous: a.previous, Consent: consent})
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return AuthorizationStart{}, errNotReady
	}
	s.attempts[a.id], s.states[a.state] = a, a.id
	s.mu.Unlock()
	id, state := a.id, a.state
	time.AfterFunc(authorizationLifetime, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.attempts, id)
		delete(s.states, state)
	})
	return AuthorizationStart{ID: a.id, AuthorizationURL: url}, nil
}

var (
	errInvalidState = &Error{Status: http.StatusBadRequest, Code: "invalid_oauth_state", Message: "Sign-in expired or the callback was already used."}
	errWrongAccount = &Error{Status: http.StatusConflict, Code: "oauth_identity_mismatch", Message: "The selected account, workspace or registration differs from this grant."}
)

func (s *Service) CompleteAuthorization(ctx context.Context, state, code, callbackClient, providerError string) error {
	s.mu.Lock()
	id := s.states[state]
	a := s.attempts[id]
	if a == nil || !time.Now().Before(a.expires) || state == "" {
		s.mu.Unlock()
		return errInvalidState
	}
	delete(s.states, state)
	s.mu.Unlock()

	var token codexupstream.Grant
	var err error
	client := s.clients[a.channel]
	if providerError != "" || code == "" {
		err = &Error{Status: http.StatusBadRequest, Code: "authorization_denied", Message: "The provider did not complete the sign-in."}
	} else {
		token, err = client.Exchange(ctx, code, a.verifier, s.redirect, a.nonce, callbackClient, a.previous)
		if err != nil {
			var upstream *codexupstream.UpstreamError
			switch {
			case errors.As(err, &upstream) && upstream.Code == "identity_mismatch":
				err = errWrongAccount
			case errors.As(err, &upstream) && upstream.Code == "codex_workspace_unverified":
				err = &Error{Status: http.StatusBadRequest, Code: "codex_workspace_unverified", Message: "The Codex authorization did not verify an upstream workspace."}
			case errors.As(err, &upstream) && upstream.Stage == "identity":
				err = &Error{Status: http.StatusBadRequest, Code: "oauth_identity_unverified", Message: "The provider's identity could not be verified. Check the account and workspace before trying again."}
			default:
				err = &Error{Status: http.StatusBadRequest, Code: "oauth_exchange_failed", Message: "Sign-in failed at the provider's token exchange."}
			}
		}
	}
	if err == nil && a.grantID != "" &&
		(token.ClientID != a.previous.ClientID || token.Subject != a.previous.Subject ||
			token.UserID != a.previous.UserID || token.AccountID != a.previous.AccountID) {
		err = errWrongAccount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts[id] != a {
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
