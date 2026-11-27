package codexupstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

var (
	ErrReauthorizationRequired   = errors.New("upstream authorization must be renewed")
	ErrRefreshOutcomeUnknown     = errors.New("upstream refresh outcome is unknown")
	ErrAuthorizationCodeRejected = errors.New("upstream rejected the OAuth authorization code")
)

type UpstreamError struct {
	StatusCode            int
	Code, Param, Stage    string
	RetryAfter, RequestID string
	Body                  []byte
	RetryAt               time.Time
}

func (e *UpstreamError) Error() string {
	switch e.Code {
	case "reauth_required":
		return ErrReauthorizationRequired.Error()
	case "refresh_outcome_unknown":
		return ErrRefreshOutcomeUnknown.Error()
	case "invalid_grant":
		return ErrAuthorizationCodeRejected.Error()
	case "identity_mismatch":
		return "authorization belongs to a different upstream user or workspace"
	case "plan_permission_required":
		return "ChatGPT plan usage is not authorized"
	case "invalid_client":
		return "the OAuth registration was rejected"
	case "authorization_rejected":
		return "upstream rejected the access credential"
	case "catalog_route_unverified":
		return "the dedicated OAuth catalog endpoint has not been established"
	case "codex_workspace_unverified":
		return "the Codex upstream workspace could not be verified"
	case "codex_route_unavailable":
		return "upstream Codex workspace routing is unavailable"
	case "model_incompatible":
		return "the account has no models compatible with this Codex client"
	case "invalid_request":
		return "the Responses request is malformed"
	case "unsupported_parameter":
		return "the selected channel or transport does not accept this parameter"
	case "unsupported_tool":
		return "ChatGPT plan usage does not support this Responses tool"
	case "unsupported_tool_encoding":
		return "ChatGPT plan usage requires local tools inside a namespace or additional_tools"
	case "interrupt_unsupported":
		return "the selected channel does not support Responses WebSocket interruption"
	default:
		return "upstream " + e.Stage + " failed"
	}
}

func (e *UpstreamError) Unwrap() error {
	switch e.Code {
	case "reauth_required":
		return ErrReauthorizationRequired
	case "refresh_outcome_unknown":
		return ErrRefreshOutcomeUnknown
	case "invalid_grant":
		return ErrAuthorizationCodeRejected
	default:
		return nil
	}
}

func invalidResponse(stage string) *UpstreamError {
	return &UpstreamError{Code: "invalid_upstream_response", Stage: stage, StatusCode: http.StatusBadGateway}
}

func unavailable(stage string) *UpstreamError {
	return &UpstreamError{Code: "upstream_unavailable", Stage: stage, StatusCode: http.StatusServiceUnavailable}
}

func identityMismatch() *UpstreamError {
	return &UpstreamError{Code: "identity_mismatch", Stage: "identity", StatusCode: http.StatusConflict}
}

func renewalRequired() *UpstreamError {
	return &UpstreamError{Code: "reauth_required", Stage: "refresh", StatusCode: http.StatusUnauthorized}
}

func refreshUnknown() *UpstreamError {
	return &UpstreamError{Code: "refresh_outcome_unknown", Stage: "refresh", StatusCode: http.StatusServiceUnavailable}
}

func transportError(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return unavailable(stage)
}

type refusalEnvelope struct {
	Error  json.RawMessage `json:"error"`
	Detail json.RawMessage `json:"detail"`
	Code   string          `json:"code"`
}

type refusalDetail struct {
	Code  string `json:"code"`
	Type  string `json:"type"`
	Param string `json:"param"`
}

func decodeRefusal(body []byte) (code, param string) {
	var envelope refusalEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return "", ""
	}
	if json.Unmarshal(envelope.Error, &code) == nil && code != "" {
		return code, ""
	}
	for _, raw := range []json.RawMessage{envelope.Error, envelope.Detail} {
		var detail refusalDetail
		if json.Unmarshal(raw, &detail) == nil {
			if detail.Code != "" {
				return detail.Code, detail.Param
			}
			if detail.Type != "" {
				return detail.Type, detail.Param
			}
		}
	}
	return envelope.Code, ""
}

func responseError(response *http.Response, stage string, exposeBody bool) *UpstreamError {
	result := &UpstreamError{
		StatusCode: response.StatusCode, Stage: stage,
		RetryAfter: response.Header.Get("Retry-After"),
		RequestID:  response.Header.Get("X-Request-Id"),
	}
	if result.RequestID == "" {
		result.RequestID = response.Header.Get("OAI-Request-Id")
	}
	if seconds, err := strconv.ParseUint(result.RetryAfter, 10, 31); err == nil {
		result.RetryAt = time.Now().Add(time.Duration(seconds) * time.Second)
	} else {
		result.RetryAt, _ = http.ParseTime(result.RetryAfter)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err == nil && len(body) <= maxResponseBytes {
		result.Code, result.Param = decodeRefusal(body)
		if exposeBody && json.Valid(body) {
			result.Body = body
		}
	}
	if result.Code == "" {
		switch {
		case response.StatusCode == http.StatusUnauthorized:
			result.Code = "authorization_rejected"
		case response.StatusCode == http.StatusTooManyRequests:
			result.Code = "rate_limited"
		case response.StatusCode >= 500:
			result.Code = "upstream_unavailable"
		default:
			result.Code = "upstream_rejected"
		}
	}
	return result
}

// ClassifyResponseError consumes a non-success inference body. The caller
// owns the response body and its read deadline.
func ClassifyResponseError(response *http.Response) *UpstreamError {
	return responseError(response, "responses", true)
}
