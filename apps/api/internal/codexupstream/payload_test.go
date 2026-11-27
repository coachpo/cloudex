package codexupstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestNormalizeResponsesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, channel, body, stream, previous, generate, metadata string
		ws                                                        bool
	}{
		{
			name: "Codex websocket", channel: ChannelCodex, ws: true,
			body:   `{"type":"response.create","model":"selected","stream":true,"store":false,"previous_response_id":"resp_1","client_metadata":{"x-codex-turn-state":"opaque"},"input":[]}`,
			stream: "true", previous: `"resp_1"`, metadata: `{"x-codex-turn-state":"opaque"}`,
		},
		{
			name: "ChatGPT websocket", channel: ChannelChatGPT, ws: true,
			body:     `{"type":"response.create","model":"selected","stream":true,"store":false,"generate":false,"previous_response_id":"resp_1","client_metadata":{"x-codex-turn-state":"opaque"},"input":[]}`,
			previous: `"resp_1"`, generate: "false",
		},
		{
			name: "Codex HTTP", channel: ChannelCodex,
			body:   `{"model":"selected","store":false,"input":[]}`,
			stream: "true",
		},
		{
			name: "ChatGPT HTTP", channel: ChannelChatGPT,
			body:   `{"model":"selected","store":true,"stream":false,"previous_response_id":null,"client_metadata":{"x-codex-turn-state":"opaque"},"input":[]}`,
			stream: "true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{config: Config{Channel: tc.channel}}
			body, err := c.NormalizePayload([]byte(tc.body), tc.ws)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{
				"model": `"selected"`, "input": "[]", "store": "false", "stream": tc.stream,
				"previous_response_id": tc.previous, "generate": tc.generate, "client_metadata": tc.metadata,
			} {
				if string(fields[key]) != value {
					t.Errorf("%s = %s, want %s", key, fields[key], value)
				}
			}
		})
	}
}

func TestChatGPTPlanLocalToolPayload(t *testing.T) {
	c := &Client{config: Config{Channel: ChannelChatGPT}}
	const input = `[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read_file"}]},{"type":"custom","name":"apply_patch"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"first"},{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"second"},{"type":"custom_tool_call_output","call_id":"call_2","output":"done"}]`
	const tools = `[{"type":"namespace","name":"extra","tools":[{"type":"custom","name":"echo"}]},{"type":"web_search"}]`
	body, err := c.NormalizePayload([]byte(`{"model":"selected","tools":`+tools+`,"input":`+input+`}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["input"]) != input || string(fields["tools"]) != tools {
		t.Fatalf("tool declarations or call history changed: %s", body)
	}
}

func TestChatGPTRejectsUnavailablePayloadCapabilities(t *testing.T) {
	c := &Client{config: Config{Channel: ChannelChatGPT}}
	for _, tc := range []struct {
		name, body, code, param string
		ws                      bool
	}{
		{"SSE continuation id", `{"input":[],"previous_response_id":"resp_old"}`, "unsupported_parameter", "previous_response_id", false},
		{"HTTP-only option", `{"type":"response.create","input":[],"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"}}`, "unsupported_parameter", "stream_options", true},
		{"named lane", `{"type":"response.create","input":[],"stream_id":"worker"}`, "unsupported_parameter", "stream_id", true},
		{"system input", `{"input":[{"type":"message","role":"system"}]}`, "unsupported_parameter", "input[0].role", false},
		{"text shorthand", `{"input":"hello"}`, "invalid_request", "input", false},
		{"temperature", `{"input":[],"temperature":0}`, "unsupported_parameter", "temperature", false},
		{"search tool", `{"input":[],"tools":[{"type":"tool_search"}]}`, "unsupported_tool", "tools[0].type", false},
		{"raw function declaration", `{"input":[],"tools":[{"type":"function","name":"read_file"}]}`, "unsupported_tool_encoding", "tools[0].type", false},
		{"hosted tool in namespace", `{"input":[],"tools":[{"type":"namespace","tools":[{"type":"mcp"}]}]}`, "unsupported_tool", "tools[0].tools[0].type", false},
		{"hosted tool in additional tools", `{"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"file_search"}]}]}`, "unsupported_tool", "input[0].tools[0].type", false},
		{"audio input", `{"input":[{"type":"message","role":"user","content":[{"type":"input_audio"}]}]}`, "unsupported_parameter", "input[0].content[0].type", false},
		{"video tool result", `{"input":[{"type":"function_call_output","output":[{"type":"input_video"}]}]}`, "unsupported_parameter", "input[0].output[0].type", false},
		{"interrupt", `{"type":"response.interrupt","response_id":"resp_active"}`, "interrupt_unsupported", "type", true},
		{"duplicate JSON keys", `{"input":[],"input":[]}`, "invalid_request", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.NormalizePayload([]byte(tc.body), tc.ws)
			var detail *UpstreamError
			if !errors.As(err, &detail) || detail.StatusCode != http.StatusBadRequest || detail.Stage != "payload" || detail.Code != tc.code || detail.Param != tc.param {
				t.Fatalf("want HTTP 400 payload error %s (%s), got %#v", tc.code, tc.param, err)
			}
		})
	}
}

func TestCodexPayloadKeepsNativeToolAndInterrupt(t *testing.T) {
	c := &Client{config: Config{Channel: ChannelCodex}}
	const body = `{"type":"response.create","model":"selected","stream":true,"store":false,"input":[],"tools":[{"type":"tool_search"}]}`
	if _, err := c.NormalizePayload([]byte(body), true); err != nil {
		t.Fatal(err)
	}
	const interruption = `{"type":"response.interrupt","response_id":"resp_active"}`
	normalized, err := c.NormalizePayload([]byte(interruption), true)
	if err != nil || string(normalized) != interruption {
		t.Fatalf("interrupt changed: %s %v", normalized, err)
	}
}
