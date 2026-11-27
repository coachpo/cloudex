package codexgateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func grantAAD(g store.Grant) []byte {
	return []byte("cloudex:managed-grant:" + g.Channel + ":" + g.RegistrationID + ":" + g.ID)
}

func (s *Service) sealGrant(g store.Grant, token codexupstream.Grant) ([]byte, error) {
	data, err := json.Marshal(token)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.key.NonceSize())
	rand.Read(nonce)
	return s.key.Seal(nonce, nonce, data, grantAAD(g)), nil
}

func (s *Service) openGrant(g store.Grant) (codexupstream.Grant, error) {
	var token codexupstream.Grant
	n := s.key.NonceSize()
	if len(g.Credential) < n {
		return token, &Error{Status: http.StatusServiceUnavailable, Code: "grant_credentials_unavailable", Message: "The grant credentials could not be opened."}
	}
	data, err := s.key.Open(nil, g.Credential[:n], g.Credential[n:], grantAAD(g))
	if err != nil {
		return token, &Error{Status: http.StatusServiceUnavailable, Code: "grant_credentials_unavailable", Message: "The grant credentials could not be opened."}
	}
	return token, json.Unmarshal(data, &token)
}

func (s *Service) grantRecord(id string, client *codexupstream.Client, token codexupstream.Grant) (store.Grant, error) {
	g := store.Grant{
		ID: id, RegistrationID: store.GrantRegistrationID(client.Channel(), client.Issuer(), token.ClientID),
		Channel: client.Channel(), ClientID: token.ClientID, Issuer: client.Issuer(), Subject: token.Subject,
		UpstreamUserID: token.UserID, UpstreamWorkspaceID: token.AccountID, DisplayName: token.DisplayName,
		Email: token.Email, Status: store.GrantActive, Scope: strings.Join(token.Scopes, " "), Resource: token.Resource,
		ExpiresAt: token.ExpiresAt,
	}
	if token.EarliestRefreshAt > 0 {
		g.EarliestRefreshAt = time.Unix(token.EarliestRefreshAt, 0)
	}
	if !client.PlanGranted(token) {
		g.Status, g.Reason = store.GrantMissingScope, "missing_inference_scope"
	}
	credential, err := s.sealGrant(g, token)
	g.Credential = credential
	return g, err
}

func checkGrant(g store.Grant) error {
	switch g.Status {
	case store.GrantActive:
		return nil
	case store.GrantMissingScope:
		return &Error{Status: http.StatusForbidden, Code: "missing_inference_scope", Message: "This ChatGPT grant lacks permission to use plan inference."}
	case store.GrantReauthRequired:
		if g.Reason == "invalid_client" {
			return &Error{Status: http.StatusUnauthorized, Code: "invalid_client", Message: "The provider rejected this grant's application registration."}
		}
		return &Error{Status: http.StatusUnauthorized, Code: "reauth_required", Message: "The grant needs to be authorized again."}
	default:
		return &Error{Status: http.StatusUnauthorized, Code: "grant_disconnected", Message: "The grant has been disconnected."}
	}
}

var errRefreshUnavailable = &Error{Status: http.StatusServiceUnavailable, Code: "refresh_unavailable", Message: "The grant could not refresh its access token."}

func (s *Service) currentToken(ctx context.Context, id string, epoch int64) (store.Grant, codexupstream.Grant, error) {
	g, err := s.Store.Grant(ctx, id)
	if err != nil {
		return g, codexupstream.Grant{}, err
	}
	if err := checkGrant(g); err != nil {
		return g, codexupstream.Grant{}, err
	}
	if epoch != g.GrantEpoch {
		return g, codexupstream.Grant{}, errGrantMoved
	}
	if tokenNeedsRefresh(g, time.Now()) {
		result := s.refreshes.DoChan(id+":"+strconv.FormatInt(epoch, 10), func() (any, error) {
			work, cancel := context.WithTimeout(s.ctx, 35*time.Second)
			defer cancel()
			return nil, s.refresh(work, id, epoch)
		})
		select {
		case <-ctx.Done():
			return g, codexupstream.Grant{}, ctx.Err()
		case r := <-result:
			if r.Err != nil {
				return g, codexupstream.Grant{}, r.Err
			}
		}
		g, err = s.Store.Grant(ctx, id)
		if err != nil {
			return g, codexupstream.Grant{}, err
		}
		if err := checkGrant(g); err != nil {
			return g, codexupstream.Grant{}, err
		}
		if g.GrantEpoch != epoch {
			return g, codexupstream.Grant{}, errGrantMoved
		}
	}
	// A valid access token remains usable during a known refresh backoff.
	if !g.ExpiresAt.IsZero() && !time.Now().Before(g.ExpiresAt) {
		return g, codexupstream.Grant{}, errRefreshUnavailable
	}
	token, err := s.openGrant(g)
	return g, token, err
}

func tokenNeedsRefresh(g store.Grant, now time.Time) bool {
	return g.RefreshAttemptID != "" || (!g.ExpiresAt.IsZero() && !now.Add(time.Minute).Before(g.ExpiresAt) &&
		!now.Before(g.EarliestRefreshAt) && !now.Before(g.RefreshRetryAt))
}

func (s *Service) refresh(ctx context.Context, id string, epoch int64) error {
	state := s.grantState(id)
	state.fence.Lock()
	g, err := s.Store.Grant(ctx, id)
	if err != nil {
		state.fence.Unlock()
		return err
	}
	if g.GrantEpoch != epoch {
		state.fence.Unlock()
		return errGrantMoved
	}
	if err := checkGrant(g); err != nil {
		state.fence.Unlock()
		return err
	}
	if !tokenNeedsRefresh(g, time.Now()) {
		state.fence.Unlock()
		return nil
	}
	if g.RefreshAttemptID != "" {
		err := s.failRefresh(ctx, g, store.GrantReauthRequired, "refresh_outcome_unknown", time.Time{})
		state.fence.Unlock()
		return err
	}
	token, err := s.openGrant(g)
	if err != nil {
		state.fence.Unlock()
		return err
	}
	g.RefreshAttemptID = store.ID()
	err = s.Store.BeginGrantRefresh(ctx, id, epoch, g.TokenVersion, g.RefreshAttemptID)
	state.fence.Unlock()
	if err != nil {
		return err
	}

	client := s.clients[g.Channel]
	next, exchangeErr := client.Refresh(ctx, token)
	// A consumed refresh token must not become retryable just because the HTTP caller timed out.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state.fence.Lock()
	defer state.fence.Unlock()
	if exchangeErr != nil {
		switch {
		case errors.Is(exchangeErr, codexupstream.ErrReauthorizationRequired):
			return s.failRefresh(save, g, store.GrantReauthRequired, "refresh_rejected", time.Time{})
		case errors.Is(exchangeErr, codexupstream.ErrRefreshOutcomeUnknown):
			return s.failRefresh(save, g, store.GrantReauthRequired, "refresh_outcome_unknown", time.Time{})
		}
		var upstream *codexupstream.UpstreamError
		retry := time.Now().Add(15 * time.Second)
		if errors.As(exchangeErr, &upstream) {
			if upstream.RetryAt.After(retry) {
				retry = upstream.RetryAt
			}
			if upstream.Code == "invalid_client" {
				return s.failRefresh(save, g, store.GrantReauthRequired, "invalid_client", time.Time{})
			}
		}
		if err := s.failRefresh(save, g, store.GrantActive, "refresh_temporarily_unavailable", retry); err != nil {
			return err
		}
		if !g.ExpiresAt.After(time.Now()) {
			return errRefreshUnavailable
		}
		return nil
	}
	if next.ClientID != token.ClientID || next.Subject != token.Subject || next.UserID != token.UserID || next.AccountID != token.AccountID ||
		!client.PlanGranted(next) || next.Resource != token.Resource || !sameScopes(next.Scopes, token.Scopes) {
		return s.failRefresh(save, g, store.GrantReauthRequired, "grant_identity_or_scope_changed", time.Time{})
	}
	replacement, err := s.grantRecord(g.ID, client, next)
	if err != nil {
		return err
	}
	replacement.GrantEpoch, replacement.TokenVersion, replacement.Scope = g.GrantEpoch, g.TokenVersion, g.Scope
	if client.Channel() == codexupstream.ChannelCodex && replacement.ExpiresAt.Equal(g.ExpiresAt) {
		// A partial Codex refresh can rotate only the refresh token. Do not
		// exchange it on every request during the proactive refresh window.
		replacement.RefreshRetryAt = maxTime(replacement.ExpiresAt, time.Now().Add(15*time.Second))
	}
	if err := s.Store.CompleteGrantRefresh(save, replacement, g.RefreshAttemptID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return errGrantMoved
		}
		return err
	}
	return nil
}

func maxTime(first, second time.Time) time.Time {
	if first.After(second) {
		return first
	}
	return second
}

func sameScopes(a, b []string) bool {
	set := func(scopes []string) map[string]struct{} {
		m := make(map[string]struct{}, len(scopes))
		for _, scope := range scopes {
			m[scope] = struct{}{}
		}
		return m
	}
	m, n := set(a), set(b)
	if len(m) != len(n) {
		return false
	}
	for scope := range m {
		if _, ok := n[scope]; !ok {
			return false
		}
	}
	return true
}

func (s *Service) failRefresh(ctx context.Context, g store.Grant, status, reason string, retry time.Time) error {
	if err := s.Store.FailGrantRefresh(ctx, g.ID, g.GrantEpoch, g.TokenVersion, g.RefreshAttemptID, status, reason, retry); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return errGrantMoved
		}
		return err
	}
	if status != store.GrantActive {
		s.cancelAdmissions("", g.ID)
	}
	g.Status = status
	return checkGrant(g)
}
