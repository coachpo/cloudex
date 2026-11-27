package codexgateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type responseProgress struct {
	id, kind, requestID string
	terminal            bool
	interrupted         bool
	calls, references   []string
	historyReferences   []string
	toolCalls           map[string]string
	toolResults         []store.SubmittedToolResult
	usage               *responseUsage
}

type responseUsage struct {
	Input       *int64 `json:"input_tokens"`
	Output      *int64 `json:"output_tokens"`
	Total       *int64 `json:"total_tokens"`
	InputDetail *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetail *struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (p *responseProgress) observe(data []byte, name string) string {
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response *struct {
			ID                string            `json:"id"`
			Output            []json.RawMessage `json:"output"`
			Usage             *responseUsage    `json:"usage"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return ""
	}
	if event.Type == "" {
		event.Type = name
	}
	if event.Type == "response.completed" && (event.Response == nil || !validGatewayID(event.Response.ID)) {
		return event.Type
	}
	if event.Response != nil {
		if validGatewayID(event.Response.ID) {
			p.id = event.Response.ID
		}
		for _, item := range event.Response.Output {
			p.observeItem(item, event.Type == "response.completed")
		}
		if event.Response.Usage != nil {
			p.usage = event.Response.Usage
		}
		p.interrupted = event.Type == "response.incomplete" && event.Response.IncompleteDetails.Reason == "interrupted"
	}
	if event.Type == "response.output_item.done" {
		p.observeItem(event.Item, true)
	}
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "error":
		p.terminal = true
		p.kind = event.Type
	}
	return event.Type
}

func (p *responseProgress) observeItem(item json.RawMessage, finalized bool) {
	call, references := responsesReferences(item)
	p.references = append(p.references, references...)
	kind := responseItemType(item)
	if finalized && call == "" && kind != "item_reference" {
		p.historyReferences = append(p.historyReferences, references...)
	}
	var fields struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(item, &fields) == nil && validGatewayID(fields.ID) {
		p.references = append(p.references, gatewayDigest("item", fields.ID))
	}
	if call == "" || isToolResult(kind) {
		return
	}
	if !slices.Contains(p.calls, call) {
		p.calls = append(p.calls, call)
	}
	// Intermediate snapshots can contain incomplete arguments and ciphertext.
	// Use the first finalized call, which Codex normally receives as item.done.
	if finalized && p.toolCalls[call] == "" {
		if p.toolCalls == nil {
			p.toolCalls = make(map[string]string)
		}
		p.toolCalls[call] = toolFingerprint(item, call)
	}
}

func (p responseProgress) audit(a *Admission, transport, model, outcome string) {
	fields := []any{
		"grant_id", a.Grant.ID, "grant_epoch", a.Grant.GrantEpoch,
		"workspace_id", a.Credential.WorkspaceID, "binding_revision", a.Credential.BindingRevision,
		"transport", transport, "model", model, "outcome", outcome,
	}
	if p.terminal {
		fields = append(fields, "upstream_terminal_event", p.kind, "terminal_written_downstream", outcome == p.kind)
	}
	// Upstream HTTP request IDs use req_<id>. Keep arbitrary headers and stream payload out of logs.
	if strings.HasPrefix(p.requestID, "req_") && validGatewayID(p.requestID) {
		fields = append(fields, "upstream_request_id", p.requestID)
	}
	if p.usage != nil {
		fields = appendUsage(fields, "input_tokens", p.usage.Input)
		fields = appendUsage(fields, "output_tokens", p.usage.Output)
		fields = appendUsage(fields, "total_tokens", p.usage.Total)
		if p.usage.InputDetail != nil {
			fields = appendUsage(fields, "cached_input_tokens", p.usage.InputDetail.Cached)
		}
		if p.usage.OutputDetail != nil {
			fields = appendUsage(fields, "reasoning_tokens", p.usage.OutputDetail.Reasoning)
		}
	}
	slog.Info("responses submission", fields...)
}

func appendUsage(fields []any, key string, value *int64) []any {
	if value != nil && *value >= 0 {
		return append(fields, key, *value)
	}
	return fields
}

func allowedResponseHeader(name string) bool {
	name = strings.ToLower(name)
	switch name {
	case "x-request-id", "retry-after", "openai-processing-ms", "openai-version", "x-codex-model",
		"x-model-name", "x-codex-model-verification", "x-codex-safety-buffering", "x-responsesapi-timing":
		return true
	}
	return strings.HasPrefix(name, "x-ratelimit-") || strings.HasPrefix(name, "x-codex-primary-") ||
		strings.HasPrefix(name, "x-codex-secondary-") || strings.HasPrefix(name, "x-codex-credits-")
}

func (g *gatewayTransport) responseHeaders(dst, src http.Header, scope requestScope, upstream Upstream, allowState bool) {
	for name, values := range src {
		if allowedResponseHeader(name) {
			for _, value := range values {
				dst.Add(name, redactUpstreamString(value, upstream.Token))
			}
		}
	}
	state := src.Get(turnStateHeader)
	if allowState && upstream.Client.Channel() == codexupstream.ChannelCodex && state != "" && len(state) <= 8192 {
		dst.Set(turnStateHeader, g.rememberTurnState(scope, state))
	}
}

func (g *gatewayTransport) responseMetadata(data []byte, scope requestScope, upstream Upstream) ([]byte, error) {
	var metadata map[string]json.RawMessage
	var headers map[string]json.RawMessage
	if json.Unmarshal(data, &metadata) != nil || json.Unmarshal(metadata["headers"], &headers) != nil {
		return data, nil
	}
	for name, value := range headers {
		if allowedResponseHeader(name) {
			continue
		}
		delete(headers, name)
		if upstream.Client.Channel() == codexupstream.ChannelCodex && strings.EqualFold(name, turnStateHeader) {
			var state string
			if json.Unmarshal(value, &state) != nil {
				var values []string
				if json.Unmarshal(value, &values) == nil && len(values) != 0 {
					state = values[0]
				}
			}
			if state != "" && len(state) <= 8192 {
				headers[strings.ToLower(turnStateHeader)], _ = json.Marshal(g.rememberTurnState(scope, state))
			}
		}
	}
	raw, _ := json.Marshal(headers)
	metadata["headers"] = redactResponseJSON(raw, upstream.Token)
	return json.Marshal(metadata)
}
