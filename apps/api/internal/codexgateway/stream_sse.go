package codexgateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func (g *gatewayTransport) sse(w http.ResponseWriter, r *http.Request) {
	a, err := g.admit(w, r)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	defer a.Release()
	body, err := readGatewayBody(w, r)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	p, err := parseResponses(body, false)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	scope, err := responsesScope(r.Header, a.Credential, p.metadata)
	if err != nil {
		writeGatewayFailure(w, err)
		return
	}
	if err := g.rejectPending(a.Context, scope, false); err != nil {
		writeGatewayFailure(w, err)
		return
	}
	for !g.openSSE(w, r, a, scope, p) {
		// A normal refresh won before the handoff. No generation was
		// reserved or written, so prepare again with the latest token.
	}
}

// openSSE returns false only when the admission fence saw a normal refresh
// before the request could be reserved or written.
func (g *gatewayTransport) openSSE(w http.ResponseWriter, r *http.Request, a *Admission, scope requestScope, p responsesPayload) bool {
	upstream, err := g.service.Upstream(a.Context, a)
	if err != nil {
		writeGatewayFailure(w, err)
		return true
	}
	catalog, err := g.service.Models(a.Context, a, upstream)
	if err != nil {
		writeGatewayFailure(w, err)
		return true
	}
	body, err := g.prepare(a, upstream, catalog, scope, p, false, "")
	if err != nil {
		writeGatewayFailure(w, err)
		return true
	}
	ctx, cancel := context.WithCancel(a.Context)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.Route.ResponsesURL, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		writeGatewayFailure(w, err)
		return true
	}
	req.ContentLength = int64(len(body))
	req.Close = true
	req.Header, err = upstream.Client.RequestHeaders(upstream.Token, upstream.Route, false)
	if err != nil {
		writeGatewayFailure(w, err)
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	codex := upstream.Client.Channel() == "codex"
	if codex {
		setCodexAffinity(req.Header, scope)
		if usesResponsesLite(catalog, p.model) {
			req.Header.Set(responsesLiteHeader, "true")
		}
	}
	state, err := g.turnState(scope, r.Header.Get(turnStateHeader))
	if err != nil {
		writeGatewayFailure(w, err)
		return true
	}
	if codex && state != "" {
		req.Header.Set(turnStateHeader, state)
	}

	result, current, err := g.startSSE(a, scope, p, upstream, req)
	if errors.Is(err, errTokenMoved) {
		return false
	}
	progress := responseProgress{toolResults: p.submittedResults}
	outcome := "submission_outcome_unknown"
	if current.Sequence != 0 {
		defer func() { progress.audit(a, "sse", redactUpstreamString(p.model, upstream.Token), outcome) }()
	}
	if err != nil {
		if a.Context.Err() != nil {
			err = invalidCredential()
		} else if current.Sequence != 0 {
			err = outcomeUnknown()
		}
		writeGatewayFailure(w, err)
		return true
	}
	response := result
	defer response.Body.Close()
	progress.requestID = redactUpstreamString(response.Header.Get("X-Request-Id"), upstream.Token)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// An explicit error is terminal for this turn, including 429. It must
		// not cause another upstream submission through a different transport.
		progress.kind = "upstream_rejected"
		if response.StatusCode == 429 {
			progress.kind = "rate_limited"
		}
		outcome = progress.kind
		_ = a.WithWrite(0, func() error { return g.complete(a.Context, scope, current, progress) })
		timer := time.AfterFunc(streamIdleTimeout, cancel)
		defer timer.Stop()
		upstreamError := redactRefusal(codexupstream.ClassifyResponseError(response), upstream.Token)
		if err := a.WithWrite(0, func() error {
			g.responseHeaders(w.Header(), response.Header, scope, upstream, false)
			writeGatewayFailure(w, upstreamError)
			return nil
		}); err != nil {
			writeGatewayFailure(w, err)
		}
		return true
	}
	contentTypes := response.Header.Values("Content-Type")
	var stream io.Reader = response.Body
	var verified bool
	switch len(contentTypes) {
	case 0:
		// Some Responses origins omit Content-Type on successful streams.
		// Confirm the framing before sending 200 and replay the bytes read
		// during inspection, including any data buffered past the first event.
		timer := time.AfterFunc(handshakeTimeout, cancel)
		stream, verified = probeResponsesSSE(response.Body)
		timer.Stop()
	case 1:
		content, _, err := mime.ParseMediaType(contentTypes[0])
		verified = err == nil && content == "text/event-stream"
	}
	if !verified {
		slog.Warn("unrecognized upstream SSE response", "status", response.StatusCode, "content_type_present", len(contentTypes) != 0)
		writeGatewayFailure(w, outcomeUnknown())
		return true
	}
	headersSent := false
	err = a.WithWrite(0, func() error {
		g.responseHeaders(w.Header(), response.Header, scope, upstream, true)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		headersSent = true
		w.WriteHeader(response.StatusCode)
		return writeGatewayChunk(w, nil, true)
	})
	if err != nil {
		if !headersSent {
			writeGatewayFailure(w, err)
		}
		return true
	}
	idle := time.AfterFunc(streamIdleTimeout, cancel)
	defer idle.Stop()
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 16<<10), maxStreamEventBytes)
	err = consumeSSE(scanner, func(line []byte) {
		idle.Reset(streamIdleTimeout)
	}, func(event sseEvent) error {
		var kind string
		eventKind, named, valid := responsesSSEKind(event)
		if valid {
			kind = progress.observe(event.data, event.kind)
		}
		if eventKind == "response.completed" && kind == "" {
			// Neither record nor forward a completion without a response ID.
			return nil
		}
		data := event.data
		if valid && named && isResponsesSSEKind(eventKind) {
			// Codex reads the JSON type independently of the SSE event name.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				return err
			}
			fields["type"], _ = json.Marshal(eventKind)
			var err error
			data, err = json.Marshal(fields)
			if err != nil {
				return err
			}
		}
		if kind == "response.metadata" {
			var err error
			data, err = g.responseMetadata(data, scope, upstream)
			if err != nil {
				return err
			}
		}
		data = redactSSEData(data, upstream.Token)
		if !bytes.Equal(data, event.data) {
			event.wire = event.wireWithData(data)
		}
		// Raw SSE fields and comments bypass the JSON redactor.
		for _, secret := range []string{upstream.Token.AccessToken, upstream.Token.RefreshToken, upstream.Token.IDToken} {
			if secret != "" && bytes.Contains(event.wire, []byte(secret)) {
				event.wire = bytes.ReplaceAll(event.wire, []byte(secret), []byte("[redacted]"))
			}
		}
		return a.WithWrite(0, func() error {
			if progress.terminal {
				if err := g.complete(a.Context, scope, current, progress); err != nil {
					return err
				}
			}
			if err := writeGatewayChunk(w, event.wire, true); err != nil {
				return err
			}
			if progress.terminal {
				outcome = progress.kind
			}
			return nil
		})
	}, func() bool { return progress.terminal })
	if progress.terminal || a.Context.Err() != nil {
		return true
	}
	if err != nil || ctx.Err() != nil {
		_ = a.WithWrite(0, func() error { return writeGatewayChunk(w, sseUnknownFrame, true) })
	}
	return true
}

func (g *gatewayTransport) startSSE(a *Admission, scope requestScope, p responsesPayload, upstream Upstream, req *http.Request) (*http.Response, store.Submission, error) {
	var current store.Submission
	ctx := req.Context()
	conn, err := dialResponsesHTTP(ctx, upstream.Client.HTTPClient(), req.URL)
	if err != nil {
		return nil, current, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	closeConn := func() {
		stop()
		_ = conn.Close()
	}
	err = a.WithWrite(upstream.TokenVersion, func() error {
		var err error
		current, err = g.reserve(a.Context, scope, p, false)
		if err != nil {
			return err
		}
		if err := conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
			return err
		}
		// Request.Write writes and flushes directly to conn here. The
		// net/http WroteRequest trace runs before Transport's final
		// buffer flush and cannot serve as this admission boundary.
		return req.Write(conn)
	})
	if err != nil {
		closeConn()
		return nil, current, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		closeConn()
		return nil, current, err
	}
	limited := &io.LimitedReader{R: conn, N: 1 << 20}
	reader := bufio.NewReader(limited)
	for {
		response, err := http.ReadResponse(reader, req)
		if err != nil {
			closeConn()
			return nil, current, err
		}
		if response.StatusCode < 200 && response.StatusCode != http.StatusSwitchingProtocols {
			continue
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			closeConn()
			return nil, current, err
		}
		limited.N = math.MaxInt64
		response.Body = &responsesBody{ReadCloser: response.Body, closeConn: closeConn}
		return response, current, nil
	}
}

type responsesBody struct {
	io.ReadCloser
	closeConn func()
}

func (body *responsesBody) Close() error {
	body.closeConn()
	return body.ReadCloser.Close()
}

func dialResponsesHTTP(ctx context.Context, client *http.Client, target *url.URL) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	var dialer net.Dialer
	dial := dialer.DialContext
	var config *tls.Config
	if transport, ok := client.Transport.(*http.Transport); ok {
		if transport.DialContext != nil {
			dial = transport.DialContext
		}
		config = transport.TLSClientConfig
	}
	port := target.Port()
	switch target.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}
	case "http":
		if !net.ParseIP(target.Hostname()).IsLoopback() {
			return nil, gatewayFailure(502, "responses_route_invalid", "", "The applied Responses route requires HTTPS.")
		}
		if port == "" {
			port = "80"
		}
	default:
		return nil, gatewayFailure(502, "responses_route_invalid", "", "The applied Responses route requires HTTPS.")
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(target.Hostname(), port))
	if err != nil || target.Scheme == "http" {
		return conn, err
	}
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		config = config.Clone()
	}
	config.ServerName = target.Hostname()
	config.InsecureSkipVerify = false
	config.NextProtos = []string{"http/1.1"}
	secure := tls.Client(conn, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return secure, nil
}

func probeResponsesSSE(body io.Reader) (io.Reader, bool) {
	const probeLimit = 64 << 10
	var prefix bytes.Buffer
	scanner := bufio.NewScanner(io.TeeReader(io.LimitReader(body, probeLimit), &prefix))
	scanner.Buffer(make([]byte, 4096), probeLimit)
	var observed, recognized bool
	err := consumeSSE(scanner, func([]byte) {}, func(event sseEvent) error {
		if len(event.data) == 0 {
			return nil
		}
		observed = true
		kind, _, valid := responsesSSEKind(event)
		recognized = valid && isResponsesSSEKind(kind)
		return nil
	}, func() bool { return observed })
	if err != nil || !recognized {
		return nil, false
	}
	return io.MultiReader(bytes.NewReader(prefix.Bytes()), body), true
}

func isResponsesSSEKind(kind string) bool {
	return strings.HasPrefix(kind, "response.") || kind == "error" || kind == "codex.rate_limits"
}

func responsesSSEKind(event sseEvent) (kind string, named, valid bool) {
	var message *struct {
		Type     string `json:"type"`
		Response *struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	err := json.Unmarshal(event.data, &message)
	if message == nil {
		return "", false, false
	}
	kind, named = message.Type, message.Type == ""
	if named {
		kind = event.kind
	}
	valid = err == nil && (kind != "response.completed" || message.Response != nil && validGatewayID(message.Response.ID))
	return kind, named, valid
}

func redactSSEData(data []byte, grant codexupstream.Grant) []byte {
	redacted := redactResponseJSON(data, grant)
	if !bytes.Equal(data, redacted) || bytes.IndexByte(data, '\\') == -1 {
		return redacted
	}
	// Scan decoded strings before merging duplicate keys so even an overwritten
	// value cannot pass through the raw event with an escaped credential.
	input := bytes.NewReader(data)
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	for {
		value, err := decoder.Token()
		if err != nil {
			return data
		}
		if value, ok := value.(string); ok && redactUpstreamString(value, grant) != value {
			break
		}
	}
	input.Reset(data)
	decoder = json.NewDecoder(input)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	redacted, _ = json.Marshal(redactResponseValue(value, grant))
	return redacted
}

type sseEvent struct {
	kind string
	data []byte
	wire []byte
}

func (e sseEvent) wireWithData(data []byte) []byte {
	wire := make([]byte, 0, len(e.wire)+len(data))
	for line := range bytes.SplitSeq(bytes.TrimSuffix(e.wire, []byte("\n\n")), []byte("\n")) {
		field, _, _ := bytes.Cut(line, []byte(":"))
		if !bytes.Equal(field, []byte("data")) {
			wire = append(append(wire, line...), '\n')
		}
	}
	wire = append(wire, "data: "...)
	return append(append(wire, data...), '\n', '\n')
}

var sseUnknownFrame = []byte("data: {\"type\":\"error\",\"error\":{\"type\":\"submission_outcome_unknown\",\"code\":\"submission_outcome_unknown\",\"message\":\"This turn may already have been submitted. Start a new turn.\"}}\n\n")

var errStreamEventTooLarge = errors.New("SSE event exceeds the gateway limit")

// SSE treats CR and LF as line endings. Consume CR immediately so a
// CR-terminated event on a still-open stream can be dispatched without waiting
// for the next byte, then discard the optional LF at the next scan.
func splitSSELines() bufio.SplitFunc {
	skipLF := false
	return func(data []byte, atEOF bool) (int, []byte, error) {
		start := 0
		if skipLF && len(data) != 0 && data[0] == '\n' {
			start = 1
			skipLF = false
		}
		if end := bytes.IndexAny(data[start:], "\r\n"); end >= 0 {
			end += start
			skipLF = data[end] == '\r'
			return end + 1, data[start:end], nil
		}
		if atEOF && len(data) > start {
			return len(data), data[start:], nil
		}
		return start, nil, nil
	}
}

func consumeSSE(scanner *bufio.Scanner, activity func([]byte), deliver func(sseEvent) error, terminal func() bool) error {
	scanner.Split(splitSSELines())
	var event sseEvent
	firstLine := true
	for scanner.Scan() {
		line := scanner.Bytes()
		if firstLine {
			line = bytes.TrimPrefix(line, []byte("\uFEFF"))
			firstLine = false
		}
		activity(line)
		if len(event.wire)+len(line)+1 > maxStreamEventBytes {
			return errStreamEventTooLarge
		}
		event.wire = append(append(event.wire, line...), '\n')
		if len(line) == 0 {
			event.data = bytes.TrimSuffix(event.data, []byte("\n"))
			if err := deliver(event); err != nil || terminal() {
				return err
			}
			event = sseEvent{}
			continue
		}
		key, value, _ := strings.Cut(string(line), ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event.kind = value
		case "data":
			event.data = append(append(event.data, value...), '\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// EOF alone does not dispatch an unfinished SSE event or prove completion.
	return io.ErrUnexpectedEOF
}
