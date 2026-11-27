package codexgateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func (s *Service) BeginDeviceAuthorization(ctx context.Context, owner, channel, grantID string) (AuthorizationStart, error) {
	if channel != codexupstream.ChannelCodex {
		return AuthorizationStart{}, &Error{Status: http.StatusBadRequest, Code: "invalid_authorization_method", Message: "Device Code is available for Sign in with Codex."}
	}
	if s.origin == "" {
		return AuthorizationStart{}, errDisabled
	}
	a, _, err := s.prepareAuthorization(ctx, owner, channel, grantID)
	if err != nil {
		return AuthorizationStart{}, err
	}
	client := s.clients[channel]
	device, err := client.RequestCodexDeviceAuthorization(ctx)
	if err != nil {
		var upstream *codexupstream.UpstreamError
		if errors.As(err, &upstream) && (upstream.StatusCode == http.StatusNotFound || upstream.StatusCode == http.StatusForbidden) {
			return AuthorizationStart{}, &Error{Status: http.StatusForbidden, Code: "device_code_unavailable", Message: "Device Code is unavailable. Check whether device code login is enabled for the OpenAI account or workspace."}
		}
		return AuthorizationStart{}, &Error{Status: http.StatusBadGateway, Code: "device_code_request_failed", Message: "Could not start device sign-in with OpenAI."}
	}
	a.expires = time.Now().Add(codexupstream.CodexDeviceAuthorizationLifetime)
	// Polling outlives the request that returns the prompt to the administrator.
	work, cancel := context.WithDeadline(s.ctx, a.expires)
	a.cancelDevice = cancel
	if err := s.trackAuthorization(a); err != nil {
		cancel()
		return AuthorizationStart{}, err
	}
	go func() {
		defer cancel()
		token, err := client.CompleteCodexDeviceAuthorization(work, device, a.previous)
		if work.Err() != nil {
			return
		}
		if err != nil {
			err = authorizationExchangeError(err)
		}
		s.finishAuthorization(a, token, err)
	}()
	return AuthorizationStart{ID: a.id, Device: &device}, nil
}

func (s *Service) CancelDeviceAuthorization(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[id]
	if a == nil || a.owner != owner || a.cancelDevice == nil || !time.Now().Before(a.expires) {
		return store.ErrMissing
	}
	delete(s.attempts, id)
	a.cancelDevice()
	return nil
}
