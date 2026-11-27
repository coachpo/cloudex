package codexupstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

const lastRFC3339UnixSecond = 253402300799

type tokenResponse struct {
	AccessToken       *string         `json:"access_token"`
	RefreshToken      *string         `json:"refresh_token"`
	IDToken           *string         `json:"id_token"`
	TokenType         string          `json:"token_type"`
	Scope             *string         `json:"scope"`
	ExpiresIn         int64           `json:"expires_in"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at"`
}

func (c *Client) submitTokens(ctx context.Context, body io.Reader, contentType string, refresh bool) (tokenResponse, time.Time, error) {
	stage := "exchange"
	var mayHaveSent atomic.Bool
	if refresh {
		stage = "refresh"
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteHeaders: func() { mayHaveSent.Store(true) },
			WroteRequest: func(httptrace.WroteRequestInfo) { mayHaveSent.Store(true) },
		})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.TokenURL, body)
	if err != nil {
		return tokenResponse{}, time.Time{}, unavailable(stage)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	now := time.Now().UTC()
	res, err := c.oauth.Do(req)
	if err != nil {
		if refresh && mayHaveSent.Load() {
			return tokenResponse{}, time.Time{}, refreshUnknown()
		}
		return tokenResponse{}, time.Time{}, transportError(ctx, stage)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		rejection := responseError(res, stage, false)
		if refresh {
			rejection = c.classifyRefreshRejection(rejection)
		}
		return tokenResponse{}, time.Time{}, rejection
	}
	var result tokenResponse
	if readJSON(res.Body, &result) != nil || result.ExpiresIn < 0 || result.ExpiresIn > lastRFC3339UnixSecond-now.Unix() {
		if refresh {
			return tokenResponse{}, time.Time{}, refreshUnknown()
		}
		return tokenResponse{}, time.Time{}, invalidResponse(stage)
	}
	return result, now, nil
}

func (c *Client) classifyRefreshRejection(e *UpstreamError) *UpstreamError {
	switch e.Code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused", "token_revoked":
		e.Code = "reauth_required"
		return e
	case "invalid_client":
		return e
	}
	if c.Channel() == ChannelCodex && e.StatusCode == http.StatusUnauthorized {
		e.Code = "reauth_required"
		return e
	}
	if e.StatusCode == http.StatusTooManyRequests ||
		e.Code == "temporarily_unavailable" && e.StatusCode >= 500 ||
		e.StatusCode == http.StatusServiceUnavailable && e.RetryAfter != "" {
		return e
	}
	if e.StatusCode < http.StatusBadRequest || e.StatusCode >= 500 || e.StatusCode == http.StatusRequestTimeout {
		return refreshUnknown()
	}
	return e
}

func (t tokenResponse) apply(previous Grant, now time.Time) (Grant, bool) {
	result := previous
	if t.AccessToken != nil {
		if !validBearer(*t.AccessToken) || t.TokenType != "" && !strings.EqualFold(t.TokenType, "bearer") {
			return Grant{}, false
		}
		result.AccessToken = *t.AccessToken
		if t.ExpiresIn > 0 {
			result.ExpiresAt = time.Unix(now.Unix()+t.ExpiresIn, 0).UTC()
		}
	}
	if t.RefreshToken != nil {
		if !validBearer(*t.RefreshToken) {
			return Grant{}, false
		}
		result.RefreshToken = *t.RefreshToken
	}
	if t.IDToken != nil {
		if !validBearer(*t.IDToken) {
			return Grant{}, false
		}
		result.IDToken = *t.IDToken
	}
	if t.Scope != nil {
		result.Scopes = strings.Fields(*t.Scope)
	}
	if t.EarliestRefreshAt != nil {
		var unix int64
		if json.Unmarshal(t.EarliestRefreshAt, &unix) != nil {
			var timestamp time.Time
			if json.Unmarshal(t.EarliestRefreshAt, &timestamp) != nil {
				return Grant{}, false
			}
			unix = timestamp.Unix()
		}
		if unix < 0 || unix > lastRFC3339UnixSecond {
			return Grant{}, false
		}
		result.EarliestRefreshAt = unix
	}
	return result, true
}

func (c *Client) revoke(ctx context.Context, endpoint string, body io.Reader, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return unavailable("revoke")
	}
	req.Header.Set("Content-Type", contentType)
	res, err := c.oauth.Do(req)
	if err != nil {
		return transportError(ctx, "revoke")
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	return responseError(res, "revoke", false)
}
