package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coder/websocket"
)

const (
	fixtureCodexModel   = "gpt-6.1-sol"
	fixtureChatGPTModel = "gpt-6.2-sol"
	fixtureSuccess      = "CLOUDEX_TOOL_CHAIN_OK"
	fixtureSeed         = "CLOUDEX_SEED=314159"
	fixtureDerived      = "CLOUDEX_DERIVED_314159"
)

// This fixture publishes native Codex ModelInfo over the private catalog and
// only public discovery fields over the ChatGPT catalog. Model capabilities
// here describe the fake upstream. The real ChatGPT adapter supplies the
// missing client profile when it handles the public discovery response.
const fixtureNativeModel = `{
	"slug": "gpt-6.1-sol",
	"display_name": "Fixture Codex model",
	"description": "Model provided by the synthetic Codex upstream",
	"default_reasoning_level": "low",
	"supported_reasoning_levels": [{"effort":"low","description":"Fixture effort"}],
	"shell_type": "unified_exec",
	"visibility": "list",
	"supported_in_api": false,
	"priority": 1,
	"base_instructions": "Execute the requested local shell tool calls.",
	"include_apps_usage_instructions": false,
	"support_verbosity": false,
	"truncation_policy": {"mode":"tokens","limit":10000},
	"context_window": 128000,
	"experimental_supported_tools": [],
	"tool_mode": "direct"
}`

type fixtureInference struct {
	Path, Transport, Identity, Model string
	Previous, ToolResult, Terminal   string
	InputTokens, OutputTokens        int
}

type fixtureOAuth struct {
	nonce, challenge, subject, redirect, account string
}

type fixtureFault struct {
	refuseUpgrade      bool
	refuseUpgradeRoute bool
	untypedSSE         bool
	dropStream         bool
	dropTerminal       bool
	beforeExchange     func(context.Context)
}

type gatewayFixture struct {
	t         *testing.T
	channel   string
	account   string
	model     string
	fault     fixtureFault
	device    func(*gatewayFixture, http.ResponseWriter, *http.Request)
	key       *rsa.PrivateKey
	server    *httptest.Server
	requests  atomic.Int32
	exchanges atomic.Int32

	mu       sync.Mutex
	codes    map[string]fixtureOAuth
	bearer   string
	catalogs int
	upgrades int
	rounds   []fixtureInference
	titles   int
}

func newGatewayFixture(t *testing.T, channel string, fault fixtureFault) *gatewayFixture {
	return newGatewayFixtureWithDevice(t, channel, fault, nil)
}

func newGatewayFixtureWithDevice(t *testing.T, channel string, fault fixtureFault, device func(*gatewayFixture, http.ResponseWriter, *http.Request)) *gatewayFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &gatewayFixture{
		t: t, channel: channel, account: "fixture-workspace-" + channel,
		model: fixtureCodexModel, fault: fault, device: device, key: key, codes: map[string]fixtureOAuth{},
	}
	if channel == codexupstream.ChannelChatGPT {
		f.model = fixtureChatGPTModel
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *gatewayFixture) adapter() *codexupstream.Client {
	f.t.Helper()
	cfg := codexupstream.OpenAICodexConfig()
	if f.channel == codexupstream.ChannelChatGPT {
		cfg = codexupstream.ChatGPTConfig()
	}
	base := f.server.URL
	cfg.Issuer = base
	cfg.AuthorizationURL = base + "/authorize"
	cfg.TokenURL = base + "/token"
	cfg.JWKSURL = base + "/keys"
	cfg.RevokeURL = base + "/revoke"
	cfg.DiscoveryURL = base + "/.well-known/openid-configuration"
	cfg.AccountsURL = base + "/accounts/check"
	cfg.ModelsURL = base + f.prefix() + "/models"
	if f.channel == codexupstream.ChannelCodex {
		cfg.ModelsURL += "?client_version=" + codexupstream.CodexClientVersion
	}
	cfg.ResponsesURL = base + f.prefix() + "/responses"
	cfg.HTTPClient = f.server.Client()
	client, err := codexupstream.New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return client
}

func (f *gatewayFixture) prefix() string {
	if f.channel == codexupstream.ChannelCodex {
		return "/backend-api/codex"
	}
	return "/v1"
}

func (f *gatewayFixture) issueCode(authorizationURL string) (state, code, issued string) {
	f.t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil || u.Scheme+"://"+u.Host != f.server.URL || u.Path != "/authorize" {
		f.t.Fatalf("authorization URL did not use the fake upstream: %v", err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" {
		f.t.Fatal("authorize omitted the PKCE or state fields")
	}
	if f.channel == codexupstream.ChannelChatGPT {
		if q.Get("client_id") != "dynamic_agent_client" || q.Get("ext_agent_host_id") == "" ||
			q.Get("resource") != codexupstream.ChatGPTResource || q.Get("nonce") == "" {
			f.t.Fatal("public ChatGPT authorize omitted dynamic registration, resource or nonce")
		}
		issued = "fixture-issued-client"
	} else if q.Get("client_id") != codexupstream.CodexClientID || q.Get("resource") != "" {
		f.t.Fatal("Codex authorize did not use the first-party request format")
	}
	code = "synthetic-code-" + q.Get("state")
	f.mu.Lock()
	f.codes[code] = fixtureOAuth{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		subject: "fixture-" + f.channel + "-subject", redirect: q.Get("redirect_uri"), account: f.account}
	f.mu.Unlock()
	return q.Get("state"), code, issued
}

func (f *gatewayFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	switch r.URL.Path {
	case "/keys":
		writeFixtureJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "fixture-key", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	case "/token":
		f.exchanges.Add(1)
		if f.fault.beforeExchange != nil {
			f.fault.beforeExchange(r.Context())
		}
		f.exchange(w, r)
	case "/api/accounts/deviceauth/usercode", "/api/accounts/deviceauth/token":
		if f.device == nil {
			f.t.Error("unexpected device authorization request")
			http.NotFound(w, r)
			return
		}
		f.device(f, w, r)
	case "/.well-known/openid-configuration":
		writeFixtureJSON(w, map[string]string{"issuer": f.server.URL, "revocation_endpoint": f.server.URL + "/revoke"})
	case "/revoke":
		w.WriteHeader(http.StatusOK)
	case "/accounts/check":
		if f.channel != codexupstream.ChannelCodex || !f.authenticated(r) {
			http.Error(w, "unexpected accounts bootstrap caller", http.StatusUnauthorized)
			return
		}
		writeFixtureJSON(w, map[string]any{"accounts": []map[string]string{{
			"id": f.account, "name": "Fixture upstream workspace",
			"workspace_backend_origin": "NO_CONSTRAINT", "account_routing_override": "NO_CONSTRAINT",
		}}})
	case f.prefix() + "/models":
		f.catalog(w, r)
	case f.prefix() + "/responses":
		f.responses(w, r)
	default:
		f.t.Errorf("%s fixture received unexpected destination %s", f.channel, r.URL.Path)
		http.NotFound(w, r)
	}
}

func writeFixtureJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func (f *gatewayFixture) exchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		http.Error(w, "expected token form", http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("code")
	f.mu.Lock()
	auth, ok := f.codes[code]
	delete(f.codes, code)
	f.mu.Unlock()
	challenge := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	client := codexupstream.CodexClientID
	resource := ""
	if f.channel == codexupstream.ChannelChatGPT {
		client = "fixture-issued-client"
		resource = codexupstream.ChatGPTResource
	}
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" ||
		r.PostForm.Get("client_id") != client || r.PostForm.Get("resource") != resource ||
		r.PostForm.Get("redirect_uri") != auth.redirect ||
		base64.RawURLEncoding.EncodeToString(challenge[:]) != auth.challenge {
		http.Error(w, "token form identity or PKCE mismatch", http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": f.server.URL, "aud": client, "sub": auth.subject, "exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(), "email": "same-display-address@example.test", "name": "Fixture holder",
	}
	access := "synthetic-" + f.channel + "-access"
	scope := "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	if f.channel == codexupstream.ChannelCodex {
		claims["https://api.openai.com/auth"] = map[string]any{
			"chatgpt_account_id": auth.account, "chatgpt_user_id": "fixture-codex-user",
		}
		access = f.sign(claims)
		scope = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	} else {
		claims["nonce"] = auth.nonce
	}
	f.mu.Lock()
	f.bearer = access
	f.mu.Unlock()
	writeFixtureJSON(w, map[string]any{
		"token_type": "Bearer", "access_token": access, "refresh_token": "synthetic-" + f.channel + "-refresh",
		"id_token": f.sign(claims), "expires_in": 3600,
		"scope": scope,
	})
}

func (f *gatewayFixture) sign(claims map[string]any) string {
	f.t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Error(err)
		return ""
	}
	message := base64.RawURLEncoding.EncodeToString([]byte(`{"kid":"fixture-key","alg":"RS256"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Error(err)
		return ""
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *gatewayFixture) authenticated(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bearer != "" && r.Header.Get("Authorization") == "Bearer "+f.bearer
}

func (f *gatewayFixture) inferenceIdentity(r *http.Request, ws bool) bool {
	if !f.authenticated(r) || r.Header.Get("Cookie") != "" || r.Header.Get("X-Cloudex-Run-ID") != "" {
		return false
	}
	if f.channel == codexupstream.ChannelCodex {
		return r.Header.Get("ChatGPT-Account-ID") == f.account &&
			(!ws || r.Header.Get("OpenAI-Beta") == "responses_websockets=2026-02-06")
	}
	return r.Header.Get("ChatGPT-Account-ID") == "" && r.Header.Get("X-OpenAI-Account-Routing-Override") == ""
}

func (f *gatewayFixture) catalog(w http.ResponseWriter, r *http.Request) {
	if !f.inferenceIdentity(r, false) || f.channel == codexupstream.ChannelCodex && r.URL.Query().Get("client_version") != codexupstream.CodexClientVersion {
		f.t.Errorf("%s catalog used incorrect identity or pinned client version", f.channel)
		http.Error(w, "wrong catalog identity", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.catalogs++
	f.mu.Unlock()
	model := json.RawMessage(fixtureNativeModel)
	if f.channel == codexupstream.ChannelChatGPT {
		model = json.RawMessage(fmt.Sprintf(`{"slug":%q,"display_name":"Fixture ChatGPT model","visibility":"list"}`, f.model))
	}
	writeFixtureJSON(w, map[string][]json.RawMessage{"models": {model}})
}

type fixtureResponseRequest struct {
	Type, Model        string
	Generate           *bool
	Stream             *bool
	Store              bool
	PreviousResponseID string `json:"previous_response_id"`
	Input              []fixtureInput
	Tools              json.RawMessage
	Text               struct {
		Format struct {
			Type   string
			Schema struct {
				Properties map[string]json.RawMessage
			}
		}
	}
}

type fixtureInput struct {
	Type   string
	CallID string `json:"call_id"`
	Output json.RawMessage
	Tools  json.RawMessage
}

func (f *gatewayFixture) responses(w http.ResponseWriter, r *http.Request) {
	ws := r.Method == http.MethodGet
	if !f.inferenceIdentity(r, ws) {
		f.t.Errorf("%s responses used incorrect identity or routing headers", f.channel)
		http.Error(w, "wrong responses identity", http.StatusUnauthorized)
		return
	}
	if ws {
		f.websocket(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var body fixtureResponseRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("bad SSE request: %v", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	lineEnd, prefix := "\n", ""
	if f.fault.untypedSSE {
		w.Header()["Content-Type"] = nil
		lineEnd, prefix = "\r", "\uFEFF"
	}
	f.respond(w, r, body, "sse", func(event []byte) error {
		var name string
		if f.fault.untypedSSE && bytes.Contains(event, []byte(`"type":"response.completed"`)) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(event, &fields); err != nil {
				return err
			}
			delete(fields, "type")
			var err error
			event, err = json.Marshal(fields)
			if err != nil {
				return err
			}
			name = "event: response.completed" + lineEnd
		}
		_, err := fmt.Fprintf(w, "%s%sdata: %s%s%s", prefix, name, event, lineEnd, lineEnd)
		prefix = ""
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		return err
	})
}

func (f *gatewayFixture) websocket(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.upgrades++
	f.mu.Unlock()
	if f.fault.refuseUpgradeRoute {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"subscription_sharing_route_not_supported"}}`)
		return
	}
	if f.fault.refuseUpgrade {
		w.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("fixture upgrade: %v", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)
	ctx, cancel := context.WithTimeout(f.t.Context(), 30*time.Second)
	defer cancel()
	for {
		kind, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var body fixtureResponseRequest
		if kind != websocket.MessageText || json.Unmarshal(payload, &body) != nil || body.Type != "response.create" {
			f.t.Error("fixture expected a text response.create")
			return
		}
		if !f.respond(w, r, body, "ws", func(event []byte) error {
			return conn.Write(ctx, websocket.MessageText, event)
		}) {
			return
		}
	}
}

func (f *gatewayFixture) respond(w http.ResponseWriter, r *http.Request, body fixtureResponseRequest, transport string, write func([]byte) error) bool {
	if body.Model != f.model || body.Store || len(body.Input) == 0 && body.PreviousResponseID == "" && body.Generate == nil ||
		transport == "sse" && (body.Stream == nil || !*body.Stream || body.PreviousResponseID != "") ||
		transport == "ws" && f.channel == codexupstream.ChannelChatGPT && body.Stream != nil {
		f.t.Errorf("%s %s request had invalid model or transport fields", f.channel, transport)
		if transport == "sse" {
			http.Error(w, "unsupported model or transport fields", http.StatusBadRequest)
		}
		return false
	}
	if body.Generate != nil && !*body.Generate {
		return write([]byte(`{"type":"response.completed","response":{"id":"response_warmup","status":"completed"}}`)) == nil
	}
	// The TUI starts a separate, hidden structured turn to name the thread.
	if body.Text.Format.Type == "json_schema" && body.Text.Format.Schema.Properties["title"] != nil {
		f.mu.Lock()
		f.titles++
		f.mu.Unlock()
		return write([]byte(`{"type":"response.output_item.done","item":{"id":"message_title","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"{\"title\":\"Run fixture tools\"}","annotations":[]}]}}`)) == nil &&
			write([]byte(`{"type":"response.completed","response":{"id":"response_title","status":"completed"}}`)) == nil
	}
	f.mu.Lock()
	round := len(f.rounds)
	identity := "fixture-chatgpt-subject/issued:fixture-issued-client"
	if f.channel == codexupstream.ChannelCodex {
		identity = "fixture-codex-user/" + f.account
	}
	f.rounds = append(f.rounds, fixtureInference{
		Path: r.URL.Path, Transport: transport, Identity: identity,
		Model: body.Model, Previous: body.PreviousResponseID,
	})
	f.mu.Unlock()
	if round > 2 {
		f.t.Error("fixture observed a duplicate generation")
		return false
	}
	responseID := fmt.Sprintf("response_%d", round+1)
	result, seedValue := "", ""
	if round > 0 {
		callID := fmt.Sprintf("call_fixture_%d", round)
		index := slices.IndexFunc(body.Input, func(item fixtureInput) bool {
			return item.Type == "function_call_output" && item.CallID == callID
		})
		expected := []string{fixtureSeed, fixtureDerived}[round-1]
		if index < 0 || !strings.Contains(string(body.Input[index].Output), expected) {
			f.t.Errorf("%s %s round %d did not continue %s with tool output containing %s", f.channel, transport, round+1, callID, expected)
			return false
		}
		if transport == "ws" && body.PreviousResponseID != fmt.Sprintf("response_%d", round) {
			f.t.Errorf("%s %s round %d got previous_response_id=%q", f.channel, transport, round+1, body.PreviousResponseID)
			return false
		}
		result = callID
		if round == 1 {
			after := strings.SplitN(string(body.Input[index].Output), "CLOUDEX_SEED=", 2)[1]
			seedValue = strings.TrimRight(after, "\"\\n ]}")
			if seedValue != "314159" {
				f.t.Error("fixture seed was missing from the first tool output")
				return false
			}
		}
	}
	if f.fault.dropStream {
		return false
	}
	if err := write(fmt.Appendf(nil, `{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, responseID)); err != nil {
		return false
	}
	var item []byte
	if round < 2 {
		var added strings.Builder
		for _, input := range body.Input {
			if input.Type == "additional_tools" {
				added.Write(input.Tools)
			}
		}
		tools := string(body.Tools) + added.String()
		if (round == 0 || transport == "sse") && !strings.Contains(tools, `"exec_command"`) {
			f.t.Errorf("%s %s catalog did not let Codex advertise the local exec_command tool", f.channel, transport)
			return false
		}
		if f.channel == codexupstream.ChannelChatGPT && (round == 0 || transport == "sse") &&
			(!strings.Contains(added.String(), `"exec_command"`) || strings.Contains(tools, `"tool_search"`)) {
			f.t.Errorf("ChatGPT did not use compatible additional_tools")
			return false
		}
		cmd := "printf 'CLOUDEX_SEED=314159'"
		if round == 1 {
			cmd = "printf 'CLOUDEX_DERIVED_%s' " + seedValue
		}
		arguments, _ := json.Marshal(map[string]any{"cmd": cmd, "yield_time_ms": 1000})
		item = fmt.Appendf(nil, `{"id":"function_%d","type":"function_call","status":"completed","call_id":"call_fixture_%d","name":"exec_command","arguments":%q}`, round+1, round+1, string(arguments))
	} else {
		if err := write(fmt.Appendf(nil, `{"type":"response.output_text.delta","item_id":"message_final","output_index":0,"content_index":0,"delta":%q}`, fixtureSuccess)); err != nil {
			return false
		}
		item = fmt.Appendf(nil, `{"id":"message_final","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}`, fixtureSuccess)
	}
	if err := write(append(append([]byte(`{"type":"response.output_item.done","item":`), item...), '}')); err != nil {
		return false
	}
	if f.fault.dropTerminal {
		return false
	}
	usageIn, usageOut := 10+round, 5+round
	terminal := fmt.Appendf(nil, `{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}}`, responseID, usageIn, usageOut, usageIn+usageOut)
	f.mu.Lock()
	f.rounds[round].ToolResult = result
	f.rounds[round].Terminal = "response.completed"
	f.rounds[round].InputTokens, f.rounds[round].OutputTokens = usageIn, usageOut
	f.mu.Unlock()
	return write(terminal) == nil
}

func (f *gatewayFixture) snapshot() (catalogs, upgrades, titles int, inference []fixtureInference) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogs, f.upgrades, f.titles, slices.Clone(f.rounds)
}

func copyFixtureBody(r io.Reader) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	return data
}
