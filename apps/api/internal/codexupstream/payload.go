package codexupstream

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
)

// NormalizePayload converts the pinned Codex client's transport envelope to the
// selected upstream dialect, leaving model and history validation to the gateway.
func (c *Client) NormalizePayload(body []byte, websocket bool) ([]byte, error) {
	var fields map[string]json.RawMessage
	if !jsontext.Value(body).IsValid() || json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, payloadError("invalid_request", "")
	}
	// The fixed client and gateway use the implicit lane. A named lane would
	// require separate submission, tool ownership and interruption state.
	if payloadValuePresent(fields["stream_id"]) {
		return nil, payloadError("unsupported_parameter", "stream_id")
	}
	if websocket {
		switch payloadString(fields["type"]) {
		case "response.create":
		case "response.interrupt":
			if !c.SupportsInterrupt() {
				return nil, payloadError("interrupt_unsupported", "type")
			}
			return body, nil
		default:
			return nil, payloadError("invalid_request", "type")
		}
	} else {
		for _, field := range []string{"type", "generate", "previous_response_id"} {
			if payloadValuePresent(fields[field]) {
				return nil, payloadError("unsupported_parameter", field)
			}
			delete(fields, field)
		}
		fields["stream"] = json.RawMessage("true")
	}
	delete(fields, "stream_id")
	fields["store"] = json.RawMessage("false")
	if c.Channel() == ChannelChatGPT {
		if err := normalizePlanPayload(fields, websocket); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

func normalizePlanPayload(fields map[string]json.RawMessage, websocket bool) error {
	for _, field := range []string{
		"access_programs", "background", "conversation", "max_output_tokens", "max_tool_calls",
		"metadata", "moderation", "multi_agent", "prompt", "prompt_cache_options", "prompt_cache_retention",
		"safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user",
	} {
		if payloadValuePresent(fields[field]) {
			return payloadError("unsupported_parameter", field)
		}
		delete(fields, field)
	}
	// Codex can attach backend-only metadata to either transport. Public plan
	// usage uses its own authenticated connection for continuity.
	delete(fields, "client_metadata")
	if websocket {
		delete(fields, "stream")
		if payloadValuePresent(fields["stream_options"]) {
			return payloadError("unsupported_parameter", "stream_options")
		}
		delete(fields, "stream_options")
	}

	var items []map[string]json.RawMessage
	if json.Unmarshal(fields["input"], &items) != nil || items == nil {
		return payloadError("invalid_request", "input")
	}
	for i, item := range items {
		path := fmt.Sprintf("input[%d]", i)
		if item == nil {
			return payloadError("invalid_request", path)
		}
		role := payloadString(item["role"])
		if role == "system" {
			return payloadError("unsupported_parameter", path+".role")
		}
		switch payloadString(item["type"]) {
		case "additional_tools":
			if role != "developer" {
				return payloadError("invalid_request", path+".role")
			}
			if err := validatePlanTools(item["tools"], path+".tools", true); err != nil {
				return err
			}
		case "tool_search_call", "tool_search_output", "image_generation_call", "file_search_call",
			"code_interpreter_call", "computer_call", "computer_call_output", "mcp_list_tools",
			"mcp_call", "mcp_approval_request", "mcp_approval_response", "configuration_update", "agent_message":
			return payloadError("unsupported_parameter", path+".type")
		}
		for _, key := range []string{"content", "output"} {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(item[key], &parts) != nil {
				continue
			}
			for j, part := range parts {
				switch payloadString(part["type"]) {
				case "input_audio", "input_video":
					return payloadError("unsupported_parameter", fmt.Sprintf("%s.%s[%d].type", path, key, j))
				}
			}
		}
	}
	if payloadValuePresent(fields["tools"]) {
		return validatePlanTools(fields["tools"], "tools", false)
	}
	return nil
}

func validatePlanTools(raw json.RawMessage, path string, additional bool) error {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return payloadError("invalid_request", path)
	}
	for i, tool := range tools {
		toolPath := fmt.Sprintf("%s[%d]", path, i)
		switch payloadString(tool["type"]) {
		case "namespace":
			var children []map[string]json.RawMessage
			if json.Unmarshal(tool["tools"], &children) != nil || children == nil {
				return payloadError("invalid_request", toolPath+".tools")
			}
			for j, child := range children {
				switch payloadString(child["type"]) {
				case "function", "custom":
				default:
					return payloadError("unsupported_tool", fmt.Sprintf("%s.tools[%d].type", toolPath, j))
				}
			}
		case "web_search", "web_search_preview":
		case "function", "custom":
			if !additional {
				return payloadError("unsupported_tool_encoding", toolPath+".type")
			}
		default:
			return payloadError("unsupported_tool", toolPath+".type")
		}
	}
	return nil
}

func payloadString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func payloadValuePresent(raw json.RawMessage) bool {
	return raw != nil && !bytes.Equal(raw, []byte("null"))
}

func payloadError(code, param string) *UpstreamError {
	return &UpstreamError{StatusCode: http.StatusBadRequest, Code: code, Param: param, Stage: "payload"}
}
