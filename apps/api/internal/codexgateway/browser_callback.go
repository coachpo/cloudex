package codexgateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

var (
	errInvalidCallbackURL = &Error{Status: http.StatusBadRequest, Code: "invalid_oauth_callback_url", Message: "Paste the complete callback URL from the browser address bar."}
	errCallbackAddress    = &Error{Status: http.StatusBadRequest, Code: "oauth_callback_address_mismatch", Message: "The callback address does not match this sign-in attempt."}
)

type manualCallback struct {
	owner, attemptID string
	address          *url.URL
}

func (s *Service) SubmitAuthorizationCallback(ctx context.Context, owner, attemptID, rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	address, err := url.Parse(rawURL)
	if err != nil || !address.IsAbs() || address.Host == "" || address.User != nil || strings.Contains(rawURL, "#") {
		return errInvalidCallbackURL
	}
	params, err := url.ParseQuery(address.RawQuery)
	if err != nil {
		return errInvalidCallbackURL
	}
	for _, name := range []string{"state", "code", "client_id", "error"} {
		if len(params[name]) > 1 {
			return errInvalidCallbackURL
		}
	}
	state, code, providerError := params.Get("state"), params.Get("code"), params.Get("error")
	if state == "" || (code == "" && providerError == "") {
		return errInvalidCallbackURL
	}
	a, err := s.consumeCallbackState(state, &manualCallback{owner: owner, attemptID: attemptID, address: address})
	if err != nil {
		return err
	}
	return s.exchangeBrowserCallback(ctx, a, code, params.Get("client_id"), providerError)
}

func (s *Service) CompleteAuthorization(ctx context.Context, state, code, callbackClient, providerError string) error {
	a, err := s.consumeCallbackState(state, nil)
	if err != nil {
		return err
	}
	return s.exchangeBrowserCallback(ctx, a, code, callbackClient, providerError)
}

func (s *Service) consumeCallbackState(state string, manual *manualCallback) (*authorizationAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.states[state]
	if manual != nil {
		a := s.attempts[manual.attemptID]
		if a == nil || a.owner != manual.owner || a.redirectURI == "" || !time.Now().Before(a.expires) {
			return nil, store.ErrMissing
		}
		registered, err := url.Parse(a.redirectURI)
		if err != nil || registered.Scheme != manual.address.Scheme || registered.Host != manual.address.Host ||
			registered.EscapedPath() != manual.address.EscapedPath() {
			return nil, errCallbackAddress
		}
		if id != a.id {
			return nil, errInvalidState
		}
	}
	a := s.attempts[id]
	if a == nil || !time.Now().Before(a.expires) || state == "" {
		return nil, errInvalidState
	}
	delete(s.states, state)
	return a, nil
}

func (s *Service) exchangeBrowserCallback(ctx context.Context, a *authorizationAttempt, code, callbackClient, providerError string) error {
	var token codexupstream.Grant
	var err error
	if providerError != "" || code == "" {
		err = &Error{Status: http.StatusBadRequest, Code: "authorization_denied", Message: "The provider did not complete the sign-in."}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		client := s.clients[a.channel]
		token, err = client.Exchange(ctx, code, a.verifier, a.redirectURI, a.nonce, callbackClient, a.previous)
		if err != nil {
			err = authorizationExchangeError(err)
		}
	}
	return s.finishAuthorization(a, token, err)
}
