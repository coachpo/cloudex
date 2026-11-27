package codexgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const (
	GatewayPath         = "/g/v1"
	gatewayRunHeader    = "X-Cloudex-Run-Id"
	turnMetadataHeader  = "X-Codex-Turn-Metadata"
	turnStateHeader     = "X-Codex-Turn-State"
	responsesLiteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"
	maxRequestBytes     = 64 << 20
	maxStreamEventBytes = 128 << 20
	handshakeTimeout    = 15 * time.Second
	streamWriteTimeout  = 10 * time.Second
	streamIdleTimeout   = 2 * time.Minute
	maxSocketLifetime   = time.Hour
)

type gatewayTransport struct {
	service *Service

	mu         sync.Mutex
	turnStates map[string]scopedTurnState
}

type scopedTurnState struct {
	credential, thread, turn, upstream string
	expires                            time.Time
}

func (s *Service) GatewayHandler() http.Handler {
	gateway := &gatewayTransport{service: s, turnStates: make(map[string]scopedTurnState)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+GatewayPath+"/models", gateway.models)
	mux.HandleFunc("GET "+GatewayPath+"/responses", gateway.websocket)
	mux.HandleFunc("POST "+GatewayPath+"/responses", gateway.sse)
	return mux
}

func (g *gatewayTransport) admit(w http.ResponseWriter, r *http.Request) (*Admission, error) {
	w.Header().Set("Cache-Control", "no-store")
	bearer := strings.Fields(r.Header.Get("Authorization"))
	if len(bearer) != 2 || !strings.EqualFold(bearer[0], "Bearer") || len(bearer[1]) > 256 {
		return nil, errCredential
	}
	return g.service.Admit(r.Context(), bearer[1])
}

func (g *gatewayTransport) models(w http.ResponseWriter, r *http.Request) {
	a, err := g.admit(w, r)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	defer a.Release()
	upstream, err := g.service.Upstream(a.Context, a)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	catalog, err := g.service.Models(a.Context, a, upstream)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	if len(catalog.Slugs) == 0 {
		writeGatewayFailure(w, gatewayFailure(409, "no_compatible_models", "", "The bound grant has no model compatible with this Codex version."))
		return
	}
	body, err := json.Marshal(struct {
		Models []json.RawMessage `json:"models"`
	}{catalog.Models})
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	writeStarted := false
	err = a.WithWrite(0, func() error {
		writeStarted = true
		w.Header().Set("Content-Type", "application/json")
		return writeGatewayChunk(w, redactResponseJSON(body, upstream.Token), false)
	})
	if err != nil && !writeStarted {
		writeGatewayFailure(w, err)
	}
}

func invalidCredential() error {
	return errCredential
}

func gatewayFailure(status int, code, param, message string) error {
	return &Error{Status: status, Code: code, Param: param, Message: message}
}

func invalidRequest(param, message string) error {
	return gatewayFailure(400, "invalid_request", param, message)
}

func outcomeUnknown() error {
	return gatewayFailure(409, "submission_outcome_unknown", "", "This turn may already have been submitted. Start a new turn.")
}

func gatewayErrorDetails(err error) (int, map[string]string) {
	status := http.StatusServiceUnavailable
	code, param, message := "gateway_unavailable", "", "The Cloudex gateway could not complete this stage."
	var gatewayError *Error
	var upstreamError *codexupstream.UpstreamError
	switch {
	case errors.As(err, &gatewayError):
		status, code, param, message = gatewayError.Status, gatewayError.Code, gatewayError.Param, gatewayError.Message
	case errors.As(err, &upstreamError):
		if upstreamError.StatusCode >= 400 && upstreamError.StatusCode <= 599 {
			status = upstreamError.StatusCode
		}
		code, param, message = upstreamError.Code, upstreamError.Param, "The upstream service rejected this stage."
	case errors.Is(err, store.ErrMissing), errors.Is(err, context.Canceled):
		return gatewayErrorDetails(errCredential)
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, "upstream_timeout", "The upstream service timed out."
	}
	details := map[string]string{"type": code, "code": code, "message": message}
	if param != "" {
		details["param"] = param
	}
	return status, details
}

func writeGatewayFailure(w http.ResponseWriter, err error) {
	status, details := gatewayErrorDetails(err)
	var upstream *codexupstream.UpstreamError
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if errors.As(err, &upstream) {
		if upstream.RetryAfter != "" {
			w.Header().Set("Retry-After", upstream.RetryAfter)
		}
		if upstream.RequestID != "" {
			w.Header().Set("X-Request-Id", upstream.RequestID)
		}
		if len(upstream.Body) != 0 && json.Valid(upstream.Body) {
			w.WriteHeader(status)
			_ = writeGatewayChunk(w, upstream.Body, false)
			return
		}
	}
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{"error": details})
	_ = writeGatewayChunk(w, body, false)
}

func redactUpstreamError(err error, grant codexupstream.Grant) error {
	var upstream *codexupstream.UpstreamError
	if !errors.As(err, &upstream) {
		return err
	}
	refusal := *upstream
	refusal.Body = redactResponseJSON(refusal.Body, grant)
	refusal.Code = redactUpstreamString(refusal.Code, grant)
	refusal.Param = redactUpstreamString(refusal.Param, grant)
	refusal.RequestID = redactUpstreamString(refusal.RequestID, grant)
	refusal.RetryAfter = redactUpstreamString(refusal.RetryAfter, grant)
	return &refusal
}

func redactUpstreamString(value string, grant codexupstream.Grant) string {
	for _, secret := range []string{grant.AccessToken, grant.RefreshToken, grant.IDToken} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func redactResponseJSON(body []byte, grant codexupstream.Grant) []byte {
	if !jsonContainsCredential(body, grant) {
		return body
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	redacted, _ := json.Marshal(redactResponseValue(value, grant))
	return redacted
}

func jsonContainsCredential(body []byte, grant codexupstream.Grant) bool {
	// Scan the wire form before decoding so normal stream events stay intact.
	for _, secret := range []string{grant.AccessToken, grant.RefreshToken, grant.IDToken} {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		if bytes.Contains(body, []byte(secret)) || bytes.Contains(body, encoded[1:len(encoded)-1]) {
			return true
		}
	}
	if bytes.IndexByte(body, '\\') == -1 {
		return false
	}
	// JSON strings can escape any character. Inspect decoded tokens before
	// merging duplicate keys so overwritten values are checked as well.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	for {
		value, err := decoder.Token()
		if err != nil {
			return false
		}
		if value, ok := value.(string); ok && redactUpstreamString(value, grant) != value {
			return true
		}
	}
}

func redactResponseValue(value any, grant codexupstream.Grant) any {
	switch item := value.(type) {
	case string:
		return redactUpstreamString(item, grant)
	case []any:
		for i := range item {
			item[i] = redactResponseValue(item[i], grant)
		}
	case map[string]any:
		redacted := make(map[string]any, len(item))
		for key, child := range item {
			redacted[redactUpstreamString(key, grant)] = redactResponseValue(child, grant)
		}
		return redacted
	}
	return value
}

func writeGatewayChunk(w http.ResponseWriter, data []byte, flush bool) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	if _, err := w.Write(data); err != nil {
		return err
	}
	if flush {
		return controller.Flush()
	}
	return nil
}

func gatewayDigest(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		// JSON string encoding keeps each component unambiguous.
		data, _ := json.Marshal(part)
		_, _ = sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func validGatewayID(id string) bool {
	if id == "" || len(id) > 160 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:", c)) {
			return false
		}
	}
	return true
}

type requestScope struct {
	credential, run, thread, turn string
	owner, sessionID              string
}

func (s requestScope) key() store.SubmissionKey {
	return store.SubmissionKey{
		CredentialDigest: s.credential,
		RunID:            gatewayDigest(s.run),
		TurnID:           gatewayDigest(s.thread, s.turn),
	}
}

func (s requestScope) session() string {
	return gatewayDigest(s.thread)
}

func (g *gatewayTransport) rejectPending(ctx context.Context, scope requestScope, warmup bool) error {
	if warmup {
		return nil
	}
	record, err := g.service.Store.Submission(ctx, scope.key())
	if errors.Is(err, store.ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Status == "submitting" {
		return outcomeUnknown()
	}
	return nil
}

func (g *gatewayTransport) reserve(ctx context.Context, scope requestScope, payload responsesPayload, continuation bool) (store.Submission, error) {
	if payload.warmup {
		return store.Submission{}, nil
	}
	prev, err := g.service.Store.Submission(ctx, scope.key())
	switch {
	case err == nil:
		advances := continuation && gatewayDigest(payload.previous) == prev.ResponseID
		if len(prev.CallIDs) != 0 {
			advances = true
			for _, expected := range prev.CallIDs {
				advances = advances && slices.Contains(payload.toolResults, expected)
			}
		}
		if prev.Status != "completed" && prev.Status != "interrupted" || prev.Model != payload.model || !advances {
			return store.Submission{}, outcomeUnknown()
		}
	case errors.Is(err, store.ErrMissing):
		prev = store.Submission{}
	default:
		return store.Submission{}, err
	}
	next, err := g.service.Store.CompareSubmission(ctx, scope.key(), prev.Sequence, store.Submission{Status: "submitting", Model: payload.model})
	if errors.Is(err, store.ErrConflict) {
		return store.Submission{}, outcomeUnknown()
	}
	return next, err
}

func (g *gatewayTransport) complete(ctx context.Context, scope requestScope, current store.Submission, event responseProgress) error {
	if current.Sequence == 0 {
		return nil // A non-generating WebSocket prewarm has no submission record.
	}
	status := "terminated"
	if event.kind == "response.completed" {
		status = "completed"
	} else if event.interrupted {
		status = "interrupted"
	}
	var response string
	if event.id != "" {
		response = gatewayDigest(event.id)
	}
	return g.service.Store.CompleteSubmission(ctx, scope.key(), current.Sequence, store.Submission{
		Status: status, Model: current.Model, ResponseID: response, CallIDs: event.calls,
	}, store.SubmissionCompletion{
		SessionDigest: scope.session(), References: event.references, ToolCalls: event.toolCalls,
		ToolResults: event.toolResults, HistoryReferences: event.historyReferences,
	})
}

func (g *gatewayTransport) rememberTurnState(scope requestScope, value string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for key, state := range g.turnStates {
		if !state.expires.After(now) {
			delete(g.turnStates, key)
		}
	}
	key := gatewayDigest(scope.credential, scope.thread, scope.turn, value)
	g.turnStates[key] = scopedTurnState{
		credential: scope.credential, thread: scope.thread, turn: scope.turn,
		upstream: value, expires: now.Add(maxSocketLifetime),
	}
	return key
}

func (g *gatewayTransport) turnState(scope requestScope, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	g.mu.Lock()
	state := g.turnStates[key]
	g.mu.Unlock()
	// A socket can be established during startup, before the client's first turn
	// is known. Such a state belongs to the socket's thread until it expires.
	if state.credential != scope.credential || state.thread != scope.thread ||
		(state.turn != "" && state.turn != scope.turn) || !state.expires.After(time.Now()) {
		return "", gatewayFailure(409, "invalid_turn_state", "x-codex-turn-state", "The upstream turn state belongs to another conversation.")
	}
	return state.upstream, nil
}

func readGatewayBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err == nil {
		return body, nil
	}
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		return nil, gatewayFailure(413, "request_too_large", "", "The Responses request exceeds the gateway limit.")
	}
	return nil, invalidRequest("", "The Responses request could not be read.")
}
