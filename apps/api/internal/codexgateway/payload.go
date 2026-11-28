package codexgateway

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"slices"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type responsesPayload struct {
	fields             map[string]json.RawMessage
	kind, model        string
	previous, response string
	metadata           map[string]string
	window             *int64
	compaction         bool
	warmup             bool
	references         []store.SubmissionReference
	toolResults        []string
	submittedResults   []store.SubmittedToolResult
}

func parseResponses(data []byte, webSocket bool) (responsesPayload, error) {
	var p responsesPayload
	if len(data) > maxRequestBytes {
		return p, gatewayFailure(413, "request_too_large", "", "The Responses request exceeds the gateway limit.")
	}
	if !jsontext.Value(data).IsValid() || json.Unmarshal(data, &p.fields) != nil || p.fields == nil {
		return p, invalidRequest("", "The Responses request must be a JSON object with unique field names.")
	}
	if webSocket {
		if _, present := p.fields["stream_id"]; present {
			return p, invalidRequest("stream_id", "Cloudex supports the single Responses stream used by this Codex version.")
		}
		if err := json.Unmarshal(p.fields["type"], &p.kind); err != nil {
			return p, invalidRequest("type", "The WebSocket message type is required.")
		}
		switch p.kind {
		case "response.interrupt":
			if json.Unmarshal(p.fields["response_id"], &p.response) != nil || !validGatewayID(p.response) {
				return p, invalidRequest("response_id", "The response to interrupt is required.")
			}
			return p, nil
		case "response.create":
		default:
			return p, invalidRequest("type", "The WebSocket message type is unsupported.")
		}
	}
	if json.Unmarshal(p.fields["model"], &p.model) != nil || p.model == "" {
		return p, invalidRequest("model", "A model from the current grant's catalog is required.")
	}
	if raw := p.fields["previous_response_id"]; raw != nil && json.Unmarshal(raw, &p.previous) != nil {
		return p, invalidRequest("previous_response_id", "The previous response ID is invalid.")
	}
	if p.previous != "" && !webSocket {
		return p, invalidRequest("previous_response_id", "HTTP streaming requires complete input history.")
	}
	if raw := p.fields["generate"]; raw != nil {
		var generate *bool
		if json.Unmarshal(raw, &generate) != nil {
			return p, invalidRequest("generate", "The generate field must be a boolean.")
		}
		p.warmup = generate != nil && !*generate
		if p.warmup && !webSocket {
			return p, invalidRequest("generate", "Prewarm is available only on WebSocket.")
		}
	}
	if raw := p.fields["client_metadata"]; raw != nil && json.Unmarshal(raw, &p.metadata) != nil {
		return p, invalidRequest("client_metadata", "The client metadata is invalid.")
	}
	// Each request carries its own turn metadata in the body. A WebSocket's
	// handshake header keeps the metadata of the request that opened it.
	if raw := p.metadata[strings.ToLower(turnMetadataHeader)]; raw != "" {
		var turn struct {
			RequestKind string `json:"request_kind"`
			Window      *int64 `json:"window_number"`
		}
		if json.Unmarshal([]byte(raw), &turn) != nil {
			return p, invalidRequest("client_metadata", "The Codex turn metadata is invalid.")
		}
		p.window, p.compaction = turn.Window, turn.RequestKind == "compaction"
	}
	if raw := p.fields["input"]; raw != nil && raw[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return p, invalidRequest("input", "The Responses input is invalid.")
		}
		if err := p.readInputReferences(items); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p *responsesPayload) readInputReferences(items []json.RawMessage) error {
	type inputCall struct {
		reference   int
		fingerprint string
	}
	calls := make(map[string]inputCall)
	for _, item := range items {
		kind := responseItemType(item)
		call, refs := responsesReferences(item)
		start := len(p.references)
		for _, digest := range refs {
			ref := store.SubmissionReference{Digest: digest}
			if call == "" && kind != "item_reference" {
				ref.HistoryDigest = digest
			}
			p.references = append(p.references, ref)
		}
		if isToolResult(kind) {
			if call == "" || slices.Contains(p.toolResults, call) {
				return invalidRequest("input", "Each tool result must contain a unique call_id.")
			}
			p.toolResults = append(p.toolResults, call)
			original := calls[call]
			result := toolFingerprint(item, call)
			p.submittedResults = append(p.submittedResults, store.SubmittedToolResult{
				CallID: call, CallFingerprint: original.fingerprint, Fingerprint: result,
			})
			// Restored history always provides the call before the result. A
			// result by itself can only use a reference in the current binding.
			// Only the call IDs inherit the pair proof. Opaque fields retain
			// their own reference checks.
			if original.fingerprint != "" && result != "" {
				history := store.GatewayToolHistoryDigest(original.fingerprint, result)
				p.references[original.reference].HistoryDigest = history
				p.references[start].HistoryDigest = history
			}
		} else if call != "" {
			calls[call] = inputCall{start, toolFingerprint(item, call)}
		}
	}
	return nil
}

func toolFingerprint(item json.RawMessage, call string) string {
	decoder := json.NewDecoder(bytes.NewReader(item))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		return ""
	}
	// Codex omits upstream metadata and tool encryption envelopes for custom
	// providers. Call IDs and any submitted opaque references are checked separately.
	delete(fields, "id")
	delete(fields, "metadata")
	delete(fields, "internal_chat_message_metadata_passthrough")
	delete(fields, "encrypted_content")
	delete(fields, "encrypted_function_args")
	for key, value := range fields {
		if value == nil {
			delete(fields, key)
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return gatewayDigest("tool-item", call, string(encoded))
}

func responseItemType(item json.RawMessage) string {
	var fields struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(item, &fields)
	return fields.Type
}

func isToolResult(kind string) bool {
	switch kind {
	case "function_call_output", "custom_tool_call_output", "tool_search_output", "local_shell_call_output",
		"computer_call_output", "mcp_call_output":
		return true
	}
	return false
}

func responsesReferences(item json.RawMessage) (call string, refs []string) {
	var object struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(item, &object) != nil {
		return "", nil
	}
	switch object.Type {
	case "function_call", "custom_tool_call", "tool_search_call", "local_shell_call", "computer_call", "mcp_call",
		"function_call_output", "custom_tool_call_output", "tool_search_output", "local_shell_call_output", "computer_call_output", "mcp_call_output":
		if object.Type == "local_shell_call" && object.CallID == "" {
			object.CallID = object.ID
		}
		if object.CallID != "" {
			call = gatewayDigest("call", object.CallID)
			refs = append(refs, call)
		}
	case "item_reference":
		refs = append(refs, gatewayDigest("item", object.ID))
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(item))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		refs = collectOpaqueReferences(refs, value)
	}
	return call, refs
}

func collectOpaqueReferences(refs []string, value any) []string {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			refs = collectOpaqueReferences(refs, item)
		}
	case map[string]any:
		for field, item := range typed {
			switch field {
			case "encrypted_content", "encrypted_function_args":
				refs = appendOpaqueReference(refs, field, item)
			case "content", "output", "action":
				refs = collectOpaqueReferences(refs, item)
			}
		}
	}
	return refs
}

func appendOpaqueReference(refs []string, field string, value any) []string {
	switch opaque := value.(type) {
	case string:
		if opaque != "" {
			refs = append(refs, gatewayDigest(field, opaque))
		}
	case []any:
		for _, fragment := range opaque {
			refs = appendOpaqueReference(refs, field, fragment)
		}
	}
	return refs
}

func hasModel(catalog codexupstream.Catalog, model string) bool {
	return slices.Contains(catalog.Slugs, model)
}

func usesResponsesLite(catalog codexupstream.Catalog, model string) bool {
	for _, raw := range catalog.Models {
		var info struct {
			Slug string `json:"slug"`
			Lite bool   `json:"use_responses_lite"`
		}
		if json.Unmarshal(raw, &info) == nil && info.Slug == model {
			return info.Lite
		}
	}
	return false
}

func responsesScope(header http.Header, credential store.GatewayCredential, metadata map[string]string) (requestScope, error) {
	scope := requestScope{credential: credential.Digest, owner: store.GatewayCredentialScope(credential), run: header.Get(gatewayRunHeader)}
	if !validGatewayID(scope.run) {
		return scope, invalidRequest("x-cloudex-run-id", "The launcher must supply a stable run ID for this Codex process.")
	}
	var turnMetadata struct {
		Thread string `json:"thread_id"`
		Turn   string `json:"turn_id"`
	}
	if raw := header.Get(turnMetadataHeader); raw != "" {
		if json.Unmarshal([]byte(raw), &turnMetadata) != nil {
			return scope, invalidRequest("x-codex-turn-metadata", "The Codex turn metadata is invalid.")
		}
	}
	scope.thread = header.Get("Thread-Id")
	if scope.thread == "" {
		scope.thread = header.Get("X-Client-Request-Id")
	}
	if scope.thread == "" {
		scope.thread = turnMetadata.Thread
	}
	if scope.thread == "" {
		scope.thread = metadata["thread_id"]
	}
	if scope.thread == "" {
		// Some startup probes omit the thread before sending any requests. Use
		// the run as one conservative scope when both transports omit a thread.
		scope.thread = scope.run
	}
	if claimed := metadata["thread_id"]; claimed != "" && claimed != scope.thread {
		return scope, invalidRequest("client_metadata.thread_id", "The message belongs to another Codex conversation.")
	}
	scope.turn = metadata["turn_id"]
	if scope.turn == "" {
		scope.turn = turnMetadata.Turn
	}
	scope.sessionID = header.Get("Session-Id")
	if scope.sessionID == "" {
		scope.sessionID = metadata["session_id"]
	}
	if scope.sessionID == "" {
		scope.sessionID = scope.thread
	}
	if !validGatewayID(scope.thread) || !validGatewayID(scope.sessionID) || scope.turn != "" && !validGatewayID(scope.turn) {
		return scope, invalidRequest("client_metadata", "The Codex conversation identifiers are invalid.")
	}
	return scope, nil
}

func (g *gatewayTransport) prepare(a *Admission, upstream Upstream, catalog codexupstream.Catalog, scope requestScope, p responsesPayload, websocket bool, state string) ([]byte, error) {
	if !hasModel(catalog, p.model) {
		return nil, gatewayFailure(400, "model_incompatible", "model", "This model is absent from the bound grant's compatible catalog.")
	}
	if len(p.references) != 0 {
		owned, err := g.service.Store.SubmissionReferences(a.Context, scope.credential, scope.session(), p.references)
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, gatewayFailure(409, "foreign_response_context", "input", "The input refers to a tool call or response state from another conversation.")
		}
	}
	delete(p.fields, "client_metadata")
	if _, present := p.fields["prompt_cache_key"]; present {
		p.fields["prompt_cache_key"], _ = json.Marshal(scopedThread(scope))
	}
	if websocket && upstream.Client.Channel() == "codex" {
		sanitized := map[string]string{"thread_id": scopedThread(scope), "session_id": gatewayDigest(scope.owner, scope.sessionID)}
		if scope.turn != "" {
			sanitized["turn_id"] = scopedTurn(scope)
		}
		if state != "" {
			sanitized[strings.ToLower(turnStateHeader)] = state
		}
		if usesResponsesLite(catalog, p.model) {
			sanitized["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
		}
		p.fields["client_metadata"], _ = json.Marshal(sanitized)
	}
	body, err := json.Marshal(p.fields)
	if err != nil {
		return nil, err
	}
	return upstream.Client.NormalizePayload(body, websocket)
}

func scopedThread(scope requestScope) string {
	return gatewayDigest(scope.owner, scope.thread)
}

func scopedTurn(scope requestScope) string {
	return gatewayDigest(scope.owner, scope.thread, scope.turn)
}

func setCodexAffinity(headers http.Header, scope requestScope) {
	headers.Set("Session-Id", gatewayDigest(scope.owner, scope.sessionID))
	headers.Set("Thread-Id", scopedThread(scope))
	headers.Set("X-Client-Request-Id", scopedThread(scope))
	if scope.turn != "" {
		metadata, _ := json.Marshal(map[string]string{"thread_id": scopedThread(scope), "turn_id": scopedTurn(scope)})
		headers.Set(turnMetadataHeader, string(metadata))
	}
}
