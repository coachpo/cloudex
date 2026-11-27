package codexgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/coder/websocket"
)

type wsFrame struct {
	kind websocket.MessageType
	body []byte
	err  error
}

type upstreamSocket struct {
	gateway  *gatewayTransport
	a        *Admission
	upstream Upstream
	catalog  codexupstream.Catalog
	ctx      context.Context
	header   http.Header
	scope    requestScope
	client   *websocket.Conn
	server   *websocket.Conn
	requests <-chan wsFrame
	events   <-chan wsFrame

	previous, previousModel, previousThread string
	requestID                               string
}

func (g *gatewayTransport) websocket(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeGatewayFailure(w, gatewayFailure(426, "websocket_required", "", "Use a WebSocket upgrade or POST a Responses request."))
		return
	}
	a, err := g.admit(w, r)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	defer a.Release()
	scope, err := responsesScope(r.Header, a.Credential, nil)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	for {
		err := g.openWebSocket(w, r, a, scope)
		if errors.Is(err, errTokenMoved) {
			// A normal refresh does not invalidate the binding. The old
			// handshake has granted no downstream access, so redial safely.
			continue
		}
		if err != nil {
			writeGatewayFailure(w, err)
		}
		return
	}
}

func (g *gatewayTransport) openWebSocket(w http.ResponseWriter, r *http.Request, a *Admission, scope requestScope) error {
	upstream, err := g.service.Upstream(a.Context, a)
	if err != nil {
		return err
	}
	catalog, err := g.service.Models(a.Context, a, upstream)
	if err != nil {
		return err
	}
	if err := a.WithWrite(upstream.TokenVersion, func() error { return nil }); err != nil {
		return err
	}
	server, response, err := dialGatewaySocket(a, upstream, scope)
	if err != nil {
		if response == nil {
			if a.Context.Err() != nil {
				return invalidCredential()
			}
			return err
		}
		defer response.Body.Close()
		var refusal *codexupstream.UpstreamError
		errors.As(err, &refusal)
		status := response.StatusCode
		canFallback := status == http.StatusSwitchingProtocols || canFallbackOnUpgradeStatus(status)
		if upstream.Client.Channel() == codexupstream.ChannelChatGPT && status == http.StatusForbidden &&
			refusal != nil && refusal.Code == "subscription_sharing_route_not_supported" {
			canFallback = true
		}
		var failure error
		if canFallback {
			// The upstream did not establish a usable socket. No generation
			// was sent, so the client can submit its full history over HTTP.
			failure = gatewayFailure(426, "upstream_websocket_unavailable", "", "The bound upstream route requires HTTP streaming.")
		} else {
			if refusal == nil {
				refusal = codexupstream.ClassifyResponseError(response)
			}
			failure = redactUpstreamError(refusal, upstream.Token)
		}
		return a.WithWrite(upstream.TokenVersion, func() error {
			g.responseHeaders(w.Header(), response.Header, scope, upstream, false)
			writeGatewayFailure(w, failure)
			return nil
		})
	}
	defer server.CloseNow()
	server.SetReadLimit(maxStreamEventBytes)
	var downstream *websocket.Conn
	acceptAttempted := false
	err = a.WithWrite(upstream.TokenVersion, func() error {
		g.responseHeaders(w.Header(), response.Header, scope, upstream, true)
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
		var err error
		acceptAttempted = true
		downstream, err = websocket.Accept(w, r, nil)
		return err
	})
	if err != nil {
		if !acceptAttempted {
			return err
		}
		return nil
	}
	defer downstream.CloseNow()
	downstream.SetReadLimit(maxRequestBytes)
	until := time.Now().Add(maxSocketLifetime)
	if tokenEnd := upstream.Token.ExpiresAt; !tokenEnd.IsZero() && tokenEnd.Before(until) {
		until = tokenEnd
	}
	ctx, cancel := context.WithDeadline(a.Context, until)
	defer cancel()
	socket := upstreamSocket{
		gateway: g, a: a, upstream: upstream, catalog: catalog, ctx: ctx, header: r.Header,
		scope: scope, client: downstream, server: server,
		requests: watchSocket(ctx, downstream), events: watchSocket(ctx, server),
		requestID: redactUpstreamString(response.Header.Get("X-Request-Id"), upstream.Token),
	}
	socket.serve()
	return nil
}

func dialGatewaySocket(a *Admission, upstream Upstream, scope requestScope) (*websocket.Conn, *http.Response, error) {
	headers, err := upstream.Client.RequestHeaders(upstream.Token, upstream.Route, true)
	if err != nil {
		return nil, nil, err
	}
	if upstream.Client.Channel() == "codex" {
		setCodexAffinity(headers, scope)
	}
	ctx, cancel := context.WithTimeout(a.Context, handshakeTimeout)
	defer cancel()
	client := *upstream.Client.HTTPClient()
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	capture := &handshakeRefusal{next: next}
	client.Transport = capture
	socket, response, err := websocket.Dial(ctx, upstream.Route.ResponsesURL, &websocket.DialOptions{
		HTTPClient: &client,
		HTTPHeader: headers,
	})
	if err != nil && capture.failure != nil {
		return nil, response, capture.failure
	}
	return socket, response, err
}

type handshakeRefusal struct {
	next    http.RoundTripper
	failure *codexupstream.UpstreamError
}

func canFallbackOnUpgradeStatus(status int) bool {
	return status == http.StatusUpgradeRequired || status == http.StatusNotImplemented ||
		status == http.StatusMethodNotAllowed || status/100 == 2
}

func (h *handshakeRefusal) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := h.next.RoundTrip(r)
	if err != nil || response.StatusCode == http.StatusSwitchingProtocols {
		return response, err
	}
	if canFallbackOnUpgradeStatus(response.StatusCode) {
		// These statuses suffice to fall back. The response body can be a live stream.
		_ = response.Body.Close()
		response.Body = http.NoBody
		return response, nil
	}
	// websocket.Dial samples error bodies up to 1024 bytes. Classify the
	// complete bounded document before that truncation so detail survives.
	h.failure = codexupstream.ClassifyResponseError(response)
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(h.failure.Body))
	return response, nil
}

func watchSocket(ctx context.Context, socket *websocket.Conn) <-chan wsFrame {
	frames := make(chan wsFrame, 1)
	go func() {
		defer close(frames)
		for {
			kind, body, err := socket.Read(ctx)
			select {
			case frames <- wsFrame{kind: kind, body: body, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func (s *upstreamSocket) serve() {
	for {
		frame, err := s.nextRequest()
		if err != nil {
			return
		}
		payload, err := parseWebSocketRequest(frame)
		if err != nil {
			_ = s.sendError(err)
			return
		}
		if payload.kind == "response.interrupt" {
			_ = s.sendError(gatewayFailure(409, "response_not_active", "response_id", "There is no active response to interrupt."))
			continue
		}
		if !s.generate(payload) {
			return
		}
	}
}

func (s *upstreamSocket) nextRequest() (wsFrame, error) {
	for {
		select {
		case <-s.ctx.Done():
			return wsFrame{}, s.ctx.Err()
		case request, ok := <-s.requests:
			if !ok || request.err != nil {
				return wsFrame{}, context.Canceled
			}
			return request, nil
		case event, ok := <-s.events:
			if !ok || event.err != nil {
				return wsFrame{}, context.Canceled
			}
			// The upstream can send usage/limits metadata before the first
			// response.create. Do not count it as generation.
			if err := s.forwardIdleEvent(event); err != nil {
				return wsFrame{}, err
			}
		}
	}
}

func (s *upstreamSocket) forwardIdleEvent(event wsFrame) error {
	var progress responseProgress
	kind := progress.observe(event.body, "")
	if kind == "response.metadata" {
		data, err := s.gateway.responseMetadata(event.body, s.scope, s.upstream)
		if err != nil {
			return err
		}
		event.body = data
	}
	if kind == "response.metadata" || kind == "codex.rate_limits" || kind == "error" {
		return s.write(s.client, event.kind, redactResponseJSON(event.body, s.upstream.Token))
	}
	return nil
}

func parseWebSocketRequest(frame wsFrame) (responsesPayload, error) {
	if frame.kind != websocket.MessageText {
		return responsesPayload{}, invalidRequest("type", "Responses WebSocket accepts JSON text messages.")
	}
	return parseResponses(frame.body, true)
}

func (s *upstreamSocket) generate(p responsesPayload) bool {
	scope, err := responsesScope(s.header, s.a.Credential, p.metadata)
	if err != nil {
		_ = s.sendError(err)
		return false
	}
	if err := s.gateway.rejectPending(s.ctx, scope, p.warmup); err != nil {
		_ = s.sendError(err)
		return false
	}
	continuation := p.previous != ""
	if continuation && (p.previous != s.previous || p.model != s.previousModel || scope.thread != s.previousThread) {
		_ = s.sendError(gatewayFailure(400, "previous_response_not_found", "previous_response_id", "The prior response is unavailable on this WebSocket connection."))
		return false
	}
	state, err := s.gateway.turnState(scope, p.metadata[strings.ToLower(turnStateHeader)])
	if err != nil {
		_ = s.sendError(err)
		return false
	}
	body, err := s.gateway.prepare(s.a, s.upstream, s.catalog, scope, p, true, state)
	if err != nil {
		_ = s.sendError(err)
		return false
	}
	var current store.Submission
	err = s.a.WithWrite(0, func() error {
		var err error
		current, err = s.gateway.reserve(s.ctx, scope, p, continuation)
		if err != nil {
			return err
		}
		return s.writePeer(s.server, websocket.MessageText, body)
	})
	progress := responseProgress{requestID: s.requestID, toolResults: p.submittedResults}
	outcome := "submission_outcome_unknown"
	if current.Sequence != 0 {
		defer func() { progress.audit(s.a, "websocket", redactUpstreamString(p.model, s.upstream.Token), outcome) }()
	}
	if err != nil {
		if current.Sequence != 0 {
			err = outcomeUnknown()
		}
		_ = s.sendError(err)
		return false
	}
	s.previous, s.previousModel, s.previousThread = "", "", ""
	if err := s.streamRound(scope, current, &progress); err != nil {
		if !progress.terminal && s.a.Context.Err() == nil {
			_ = s.sendError(err)
		}
		return false
	}
	outcome = progress.kind
	switch progress.kind {
	case "response.completed":
		s.previous, s.previousModel, s.previousThread = progress.id, p.model, scope.thread
		return true
	case "response.incomplete":
		if progress.interrupted {
			s.previous, s.previousModel, s.previousThread = progress.id, p.model, scope.thread
			return true
		}
	}
	return false
}

func (s *upstreamSocket) streamRound(scope requestScope, current store.Submission, progress *responseProgress) error {
	idle := time.NewTimer(streamIdleTimeout)
	defer idle.Stop()
	for !progress.terminal {
		select {
		case <-idle.C:
			return outcomeUnknown()
		case <-s.ctx.Done():
			return outcomeUnknown()
		case event, ok := <-s.events:
			if !ok || event.err != nil || event.kind != websocket.MessageText {
				return outcomeUnknown()
			}
			idle.Reset(streamIdleTimeout)
			if err := s.forwardRoundEvent(event, scope, current, progress); err != nil {
				return err
			}
		case frame, ok := <-s.requests:
			if !ok || frame.err != nil {
				return outcomeUnknown()
			}
			payload, err := parseWebSocketRequest(frame)
			if err != nil {
				return err
			}
			if payload.kind != "response.interrupt" {
				return gatewayFailure(409, "response_active", "type", "Wait for the current response before submitting another generation.")
			}
			if err := s.interrupt(payload, progress.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *upstreamSocket) forwardRoundEvent(event wsFrame, scope requestScope, current store.Submission, progress *responseProgress) error {
	kind := progress.observe(event.body, "")
	if kind == "response.completed" && !progress.terminal {
		// Completion must supply its own valid response ID.
		return nil
	}
	if kind == "response.metadata" {
		data, err := s.gateway.responseMetadata(event.body, scope, s.upstream)
		if err != nil {
			return err
		}
		event.body = data
	}
	event.body = redactResponseJSON(event.body, s.upstream.Token)
	return s.a.WithWrite(0, func() error {
		if progress.terminal {
			if err := s.gateway.complete(s.ctx, scope, current, *progress); err != nil {
				return err
			}
		}
		return s.writePeer(s.client, event.kind, event.body)
	})
}

func (s *upstreamSocket) interrupt(p responsesPayload, activeID string) error {
	if activeID == "" || p.response != activeID {
		return s.sendError(gatewayFailure(409, "response_not_active", "response_id", "This response is not active on the WebSocket connection."))
	}
	if !s.upstream.Client.SupportsInterrupt() {
		return gatewayFailure(409, "interruption_outcome_unknown", "", "The upstream connection was closed without interruption confirmation.")
	}
	body, _ := json.Marshal(map[string]string{"type": "response.interrupt", "response_id": activeID})
	if err := s.write(s.server, websocket.MessageText, body); err != nil {
		return gatewayFailure(409, "interruption_outcome_unknown", "", "The upstream connection was closed without interruption confirmation.")
	}
	return nil
}

func (s *upstreamSocket) sendError(err error) error {
	status, details := gatewayErrorDetails(err)
	wire, _ := json.Marshal(struct {
		Type   string            `json:"type"`
		Status int               `json:"status"`
		Error  map[string]string `json:"error"`
	}{"error", status, details})
	return s.write(s.client, websocket.MessageText, wire)
}

func (s *upstreamSocket) write(socket *websocket.Conn, kind websocket.MessageType, data []byte) error {
	return s.a.WithWrite(0, func() error {
		return s.writePeer(socket, kind, data)
	})
}

func (s *upstreamSocket) writePeer(socket *websocket.Conn, kind websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, streamWriteTimeout)
	defer cancel()
	return socket.Write(ctx, kind, data)
}
