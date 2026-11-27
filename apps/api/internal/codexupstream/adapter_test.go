package codexupstream

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testRedirect = "http://127.0.0.1:1455/auth/callback"
const testIssuedClient = "app_test_issued"

func fakeClient(t *testing.T, channel, base string) Config {
	t.Helper()
	c := OpenAICodexConfig()
	if channel == ChannelChatGPT {
		c = ChatGPTConfig()
	}
	for _, endpoint := range []*string{&c.Issuer, &c.AuthorizationURL, &c.TokenURL, &c.JWKSURL,
		&c.RevokeURL, &c.DiscoveryURL, &c.AccountsURL, &c.ModelsURL, &c.ResponsesURL} {
		if *endpoint == "" {
			continue
		}
		u, err := url.Parse(*endpoint)
		if err != nil {
			t.Fatal(err)
		}
		*endpoint = base + u.EscapedPath()
		if u.RawQuery != "" {
			*endpoint += "?" + u.RawQuery
		}
	}
	return c
}

func makeClient(t *testing.T, config Config) *Client {
	t.Helper()
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

type testKey struct {
	private *rsa.PrivateKey
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{private: key}
}

func (k testKey) jwks(w http.ResponseWriter) {
	encoding := base64.RawURLEncoding
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kid": "one", "kty": "RSA", "alg": "RS256", "use": "sig",
		"n": encoding.EncodeToString(k.private.PublicKey.N.Bytes()),
		"e": encoding.EncodeToString(big.NewInt(int64(k.private.PublicKey.E)).Bytes()),
	}}})
}

func (k testKey) token(issuer, audience, subject, nonce, user, workspace string) string {
	encoding := base64.RawURLEncoding
	header := encoding.EncodeToString([]byte(`{"alg":"RS256","kid":"one"}`))
	claims := map[string]any{
		"iss": issuer, "aud": audience, "sub": subject, "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce,
		"name": "Admin", "email": "same@example.invalid", "email_verified": true,
	}
	if user != "" {
		claims["https://api.openai.com/auth"] = map[string]string{"chatgpt_user_id": user, "chatgpt_account_id": workspace}
	}
	if audience == ChatGPTResource {
		claims["client_id"] = testIssuedClient
	}
	encoded, _ := json.Marshal(claims)
	payload := header + "." + encoding.EncodeToString(encoded)
	digest := sha256.Sum256([]byte(payload))
	signature, _ := rsa.SignPKCS1v15(rand.Reader, k.private, crypto.SHA256, digest[:])
	return payload + "." + encoding.EncodeToString(signature)
}

func TestChannelOAuthContracts(t *testing.T) {
	for _, channel := range []string{ChannelCodex, ChannelChatGPT} {
		t.Run(channel, func(t *testing.T) {
			ctx := t.Context()
			key := newTestKey(t)
			var exchanges atomic.Int32
			var refreshes atomic.Int32
			var revokes atomic.Int32
			clientID := CodexClientID
			nonce := ""
			tokenPath, revokePath := "/oauth/token", "/oauth/revoke"
			user, workspace, audience := "codex-user", "codex-workspace", "codex-resource"
			if channel == ChannelChatGPT {
				clientID, nonce = testIssuedClient, "saved-nonce"
				tokenPath, revokePath = "/api/accounts/oauth/token", "/api/accounts/oauth/revoke"
				user, workspace, audience = "", "", ChatGPTResource
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := "http://" + r.Host
				switch r.URL.Path {
				case "/.well-known/jwks.json":
					key.jwks(w)
				case "/.well-known/openid-configuration":
					writeJSON(w, map[string]string{"issuer": base, "revocation_endpoint": base + revokePath})
				case tokenPath:
					if r.Method != http.MethodPost {
						t.Error("token grant was not POST")
					}
					if r.Header.Get("Content-Type") == "application/json" {
						var params map[string]string
						_ = json.NewDecoder(r.Body).Decode(&params)
						refreshes.Add(1)
						if channel != ChannelCodex || params["client_id"] != clientID || params["refresh_token"] != "original-refresh" ||
							params["refresh_reason"] != "proactive_expiry" || params["grant_type"] != "refresh_token" || params["resource"] != "" {
							t.Errorf("wrong Codex refresh encoding")
						}
						// Codex can return just one member of the rotated set.
						writeJSON(w, map[string]any{"refresh_token": "rotated-refresh"})
						return
					}
					_ = r.ParseForm()
					if r.Header.Get("Content-Type") != formContentType || r.Form.Get("client_id") != clientID ||
						(channel == ChannelCodex && r.Form.Get("resource") != "") ||
						(channel == ChannelChatGPT && r.Form.Get("resource") != ChatGPTResource) {
						t.Error("wrong channel token form envelope")
					}
					if r.Form.Get("grant_type") == "refresh_token" {
						refreshes.Add(1)
						if channel != ChannelChatGPT || r.Form.Get("refresh_token") != "original-refresh" || r.Form.Has("scope") {
							t.Error("wrong ChatGPT refresh form")
						}
						writeJSON(w, map[string]any{
							"access_token": key.token(base, audience, "account-a", "", "", ""),
							"token_type":   "Bearer", "refresh_token": "rotated-refresh",
							"expires_in": 3600, "earliest_refresh_at": time.Now().Unix() + 3000,
						})
						return
					}
					exchanges.Add(1)
					if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "verifier" ||
						r.Form.Get("redirect_uri") != testRedirect || r.Form.Has("client_secret") {
						t.Error("wrong authorization-code form")
					}
					var subject, responseNonce = "account-a", nonce
					userID, accountID := user, workspace
					if r.Form.Get("code") == "other-account" {
						subject = "account-b"
					}
					if r.Form.Get("code") == "other-workspace" {
						accountID = "workspace-elsewhere"
					}
					if r.Form.Get("code") == "bad-nonce" {
						responseNonce = "different-nonce"
					}
					idToken := key.token(base, clientID, subject, responseNonce, userID, accountID)
					if r.Form.Get("code") == "bad-signature" {
						headerAndClaims, sig, _ := strings.Cut(idToken, ".")
						claims, sig, _ := strings.Cut(sig, ".")
						badByte := byte('A')
						if sig[0] == badByte {
							badByte = 'B'
						}
						idToken = headerAndClaims + "." + claims + "." + string(badByte) + sig[1:]
					}
					payload := map[string]any{
						"id_token":      idToken,
						"access_token":  key.token(base, audience, subject, "", userID, accountID),
						"refresh_token": "original-refresh", "expires_in": 3600, "token_type": "Bearer",
					}
					if channel == ChannelChatGPT {
						payload["scope"] = chatGPTScope
						if r.Form.Get("code") == "identity-only" {
							payload = map[string]any{"id_token": payload["id_token"], "scope": "profile openid email"}
						}
					}
					writeJSON(w, payload)
				case revokePath:
					revokes.Add(1)
					if channel == ChannelCodex && r.Header.Get("Content-Type") == "application/json" {
						var params map[string]string
						_ = json.NewDecoder(r.Body).Decode(&params)
						if params["token"] != "rotated-refresh" || params["client_id"] != clientID || params["token_type_hint"] != "refresh_token" {
							t.Error("wrong Codex revoke JSON")
						}
					} else if channel == ChannelChatGPT && r.Header.Get("Content-Type") == formContentType {
						_ = r.ParseForm()
						if r.Form.Get("token") != "rotated-refresh" || r.Form.Get("client_id") != clientID || r.Form.Get("token_type_hint") != "refresh_token" {
							t.Error("wrong ChatGPT revoke form")
						}
					} else {
						t.Error("wrong revoke encoding")
					}
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected upstream path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := makeClient(t, fakeClient(t, channel, server.URL))

			flow := Authorization{State: "state", Challenge: "challenge", Nonce: "nonce", Redirect: testRedirect, AgentHostID: "urn:uuid:host"}
			loginURL, _ := url.Parse(client.AuthorizeURL(flow))
			query := loginURL.Query()
			if query.Get("state") != flow.State || query.Get("code_challenge_method") != "S256" ||
				query.Get("code_challenge") != flow.Challenge || query.Get("redirect_uri") != flow.Redirect {
				t.Fatal("authorization missing PKCE, state or exact callback")
			}
			if channel == ChannelCodex {
				if query.Get("scope") != codexScope || query.Get("client_id") != CodexClientID || query.Has("nonce") ||
					query.Has("resource") || query.Get("originator") != originator || query.Get("codex_cli_simplified_flow") != "true" {
					t.Fatal("incorrect Codex authorization")
				}
			} else if query.Get("scope") != chatGPTScope || query.Get("client_id") != dynamicClientID ||
				query.Get("resource") != ChatGPTResource || query.Get("nonce") != flow.Nonce ||
				query.Get("ext_agent_host_id") != flow.AgentHostID || query.Get("agent_name_hint") != "Cloudex" {
				t.Fatal("incorrect dynamic ChatGPT registration")
			}

			callbackID := ""
			if channel == ChannelChatGPT {
				callbackID = clientID
			}
			grant, err := client.Exchange(ctx, "valid", "verifier", testRedirect, nonce, callbackID, Grant{})
			if err != nil || grant.Subject != "account-a" || grant.UserID != user || grant.AccountID != workspace ||
				grant.ClientID != clientID || grant.Email != "same@example.invalid" || !client.PlanGranted(grant) {
				t.Fatalf("failed valid exchange: %v", err)
			}
			flow.Previous = grant
			returnURL, _ := url.Parse(client.AuthorizeURL(flow))
			if channel == ChannelChatGPT && (returnURL.Query().Has("agent_name_hint") ||
				returnURL.Query().Get("client_id") != clientID || returnURL.Query().Get("id_token_hint") != grant.IDToken) {
				t.Fatal("reauthorization did not reuse the issued registration")
			}
			if _, err := client.Exchange(ctx, "other-account", "verifier", testRedirect, nonce, "", grant); errorCode(err) != "identity_mismatch" {
				t.Fatalf("wrong subject accepted: %v", err)
			}
			if _, err := client.Exchange(ctx, "valid", "verifier", testRedirect, nonce, "wrong-client", grant); errorCode(err) != "identity_mismatch" {
				t.Fatalf("wrong callback client accepted: %v", err)
			}
			if exchanges.Load() != 2 {
				t.Fatalf("first two attempts used unexpected request count %d", exchanges.Load())
			}
			if _, err := client.Exchange(ctx, "bad-signature", "verifier", testRedirect, nonce, callbackID, Grant{}); errorCode(err) != "invalid_upstream_response" {
				t.Fatalf("invalid signature accepted: %v", err)
			}
			if channel == ChannelCodex {
				if _, err := client.Exchange(ctx, "other-workspace", "verifier", testRedirect, nonce, "", grant); errorCode(err) != "identity_mismatch" {
					t.Fatalf("different workspace accepted for the same subject: %v", err)
				}
			}

			if channel == ChannelChatGPT {
				denied, err := client.Exchange(ctx, "identity-only", "verifier", testRedirect, nonce, callbackID, Grant{})
				if err != nil || denied.Subject != "account-a" || client.PlanGranted(denied) {
					t.Fatalf("identity-only permission result: %v", err)
				}
				if _, err := client.ResolveRoute(ctx, denied); errorCode(err) != "plan_permission_required" {
					t.Fatalf("identity-only credential admitted: %v", err)
				}
				if _, err := client.Exchange(ctx, "bad-nonce", "verifier", testRedirect, nonce, callbackID, Grant{}); errorCode(err) != "invalid_upstream_response" {
					t.Fatalf("wrong nonce accepted: %v", err)
				}
			}

			renewed, err := client.Refresh(ctx, grant)
			if err != nil || renewed.RefreshToken != "rotated-refresh" || renewed.Subject != grant.Subject ||
				renewed.IDToken != grant.IDToken || !slices.Equal(renewed.Scopes, grant.Scopes) {
				t.Fatalf("failed renewal: %v", err)
			}
			if channel == ChannelCodex && renewed.AccessToken != grant.AccessToken || channel == ChannelChatGPT && renewed.EarliestRefreshAt == 0 {
				t.Fatal("renewal did not merge channel-specific token fields")
			}
			if err := client.Revoke(ctx, renewed); err != nil {
				t.Fatal(err)
			}
			if refreshes.Load() != 1 || revokes.Load() != 1 {
				t.Fatal("expected one refresh and one revoke to that channel")
			}
		})
	}
}

func TestRefreshFailureSemantics(t *testing.T) {
	responses := []struct {
		name     string
		status   int
		body     string
		sentinel error
		code     string
	}{
		{"revoked", 400, `{"error":"invalid_grant"}`, ErrReauthorizationRequired, "reauth_required"},
		{"registration", 401, `{"error":"invalid_client"}`, nil, "invalid_client"},
		{"rate limited", 429, `{"error":{"code":"rate_limit_exceeded"}}`, nil, "rate_limit_exceeded"},
		{"opaque 500", 500, "", ErrRefreshOutcomeUnknown, "refresh_outcome_unknown"},
		{"truncated 200", 200, `{"access_token":`, ErrRefreshOutcomeUnknown, "refresh_outcome_unknown"},
		{"post received then disconnected", 0, "", ErrRefreshOutcomeUnknown, "refresh_outcome_unknown"},
	}
	for _, tc := range responses {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_ = r.ParseForm()
				if tc.status == 0 {
					conn, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := makeClient(t, fakeClient(t, ChannelChatGPT, server.URL))
			_, err := client.Refresh(t.Context(), Grant{ClientID: testIssuedClient, Resource: ChatGPTResource, RefreshToken: "temporary-test-value"})
			if err == nil || errorCode(err) != tc.code || tc.sentinel != nil && !errors.Is(err, tc.sentinel) || requests.Load() != 1 {
				t.Fatalf("refresh failure classified as %v, code %q, calls %d", err, errorCode(err), requests.Load())
			}
		})
	}
}

func errorCode(err error) string {
	var response *UpstreamError
	if errors.As(err, &response) {
		return response.Code
	}
	return ""
}

func TestChannelCatalogRoutingAndHeaders(t *testing.T) {
	type scenario struct {
		channel, path, clientID, slug, bearer, accountID string
	}
	scenarios := []scenario{
		{ChannelCodex, "/backend-api/codex/models", CodexClientID, "test-codex-native", "codex-access", "workspace-1"},
		{ChannelChatGPT, "/v1/models", testIssuedClient, "gpt-6.1-sol", "chatgpt-access", ""},
	}
	for _, tc := range scenarios {
		t.Run(tc.channel, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+tc.bearer || r.Header.Get("ChatGPT-Account-ID") != tc.accountID {
					t.Error("request carried incorrect auth context")
				}
				switch r.URL.Path {
				case "/backend-api/wham/accounts/check":
					if tc.channel != ChannelCodex {
						t.Error("unexpected bootstrap for ChatGPT")
					}
					writeJSON(w, map[string]any{"accounts": []codexAccount{{
						ID: tc.accountID, BackendOrigin: "NO_CONSTRAINT", RoutingOverride: "NO_CONSTRAINT", Name: "Upstream workspace",
					}}})
				case tc.path:
					model := map[string]any{"slug": tc.slug, "display_name": "Current model", "visibility": "list"}
					if tc.channel == ChannelCodex {
						model["shell_type"] = "shell_command"
						model["base_instructions"] = "Use the available local tools."
						model["experimental_supported_tools"] = []string{}
						model["supported_reasoning_levels"] = []string{}
						model["truncation_policy"] = map[string]any{"mode": "bytes", "limit": 10000}
						model["support_verbosity"] = false
						model["supported_in_api"] = false
						model["use_responses_lite"] = true
						model["priority"] = 1
						if r.URL.Query().Get("client_version") != CodexClientVersion || r.Header.Get("Originator") != "cloudex" {
							t.Error("Codex catalog missing pinned app metadata")
						}
					} else if r.Header.Get("Originator") != "" {
						t.Error("ChatGPT catalog received Codex metadata")
					}
					writeJSON(w, map[string]any{"models": []map[string]any{
						{"slug": "unknown-plan-model", "display_name": "Unverified", "visibility": "list"},
						{"slug": "gpt-6.2-sol", "display_name": "Hidden", "visibility": "hide"},
						model,
					}})
				default:
					t.Errorf("unexpected catalog path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			config := fakeClient(t, tc.channel, server.URL)
			client := makeClient(t, config)
			grant := Grant{AccessToken: tc.bearer, ClientID: tc.clientID, UserID: "user-1", AccountID: tc.accountID}
			if tc.channel == ChannelChatGPT {
				grant.Resource, grant.Scopes = ChatGPTResource, strings.Fields(chatGPTScope)
			}
			route, err := client.ResolveRoute(t.Context(), grant)
			if err != nil || route.ResponsesURL != config.ResponsesURL || route.CatalogURL != config.ModelsURL {
				t.Fatalf("resolve route: %v", err)
			}
			catalog, err := client.ListModels(t.Context(), grant, route)
			if err != nil || !slices.Equal(catalog.Slugs, []string{tc.slug}) {
				t.Fatalf("catalog slugs %v, error %v", catalog.Slugs, err)
			}
			var metadata struct {
				SupportedInAPI bool   `json:"supported_in_api"`
				UseLite        bool   `json:"use_responses_lite"`
				ToolMode       string `json:"tool_mode"`
			}
			if json.Unmarshal(catalog.Models[0], &metadata) != nil || !metadata.SupportedInAPI || !metadata.UseLite ||
				(tc.channel == ChannelChatGPT && metadata.ToolMode != "direct") {
				t.Fatal("model capabilities incompatible with provider and channel")
			}
			for _, ws := range []bool{false, true} {
				headers, err := client.RequestHeaders(grant, route, ws)
				if err != nil || headers.Get("Authorization") != "Bearer "+tc.bearer || headers.Get("ChatGPT-Account-ID") != tc.accountID {
					t.Fatalf("inference headers: %v", err)
				}
				if ws && tc.channel == ChannelCodex && headers.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" ||
					tc.channel == ChannelChatGPT && (headers.Get("OpenAI-Beta") != "" || headers.Get("Originator") != "") ||
					!ws && headers.Get("Accept") != "text/event-stream" {
					t.Error("wrong inference protocol headers")
				}
			}
		})
	}
}

func TestDedicatedCodexCatalogRequiresIndependentRoute(t *testing.T) {
	var catalogRequests atomic.Int32
	dedicated := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogRequests.Add(1)
		if r.URL.Path != "/independent/models" || r.Header.Get("ChatGPT-Account-ID") != "selected" ||
			r.Header.Get("X-Openai-Account-Routing-Override") != "us" {
			t.Error("catalog did not use verified destination and workspace routing")
		}
		writeJSON(w, map[string]any{"models": []any{}})
	}))
	defer dedicated.Close()
	var bootstrapRequests atomic.Int32
	bootstrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bootstrapRequests.Add(1)
		if r.URL.Path != "/backend-api/wham/accounts/check" ||
			r.Header.Get("X-Openai-Account-Routing-Override") != "" {
			t.Error("bootstrap used an unverified workspace or derived override")
		}
		if r.Header.Get("ChatGPT-Account-ID") == "missing" {
			writeJSON(w, map[string]any{"accounts": []codexAccount{}})
			return
		}
		if r.Header.Get("ChatGPT-Account-ID") != "selected" {
			t.Error("bootstrap did not use the workspace from the grant")
		}
		writeJSON(w, map[string]any{"accounts": []codexAccount{
			{ID: "different", BackendOrigin: "NO_CONSTRAINT", RoutingOverride: "NO_CONSTRAINT"},
			{ID: "selected", BackendOrigin: dedicated.URL, RoutingOverride: "us", Name: "Selected workspace"},
		}})
	}))
	defer bootstrap.Close()
	config := fakeClient(t, ChannelCodex, bootstrap.URL)
	config.HTTPClient = dedicated.Client()
	grant := Grant{AccessToken: "test-codex-access", ClientID: CodexClientID, UserID: "user", AccountID: "selected"}
	client := makeClient(t, config)
	route, err := client.ResolveRoute(t.Context(), grant)
	if err != nil || route.ResponsesURL != dedicated.URL+"/backend-api/codex/responses" || route.CatalogURL != "" || route.WorkspaceName != "Selected workspace" {
		t.Fatalf("dedicated route: %+v, %v", route, err)
	}
	if _, err := client.ListModels(t.Context(), grant, route); errorCode(err) != "catalog_route_unverified" {
		t.Fatalf("unverified dedicated catalog should fail at catalog stage: %v", err)
	}
	if catalogRequests.Load() != 0 || bootstrapRequests.Load() != 1 {
		t.Fatal("unverified catalog was probed")
	}
	config.CatalogRoutes = map[string]string{dedicated.URL: dedicated.URL + "/independent/models"}
	client = makeClient(t, config)
	route, err = client.ResolveRoute(t.Context(), grant)
	if err != nil || route.CatalogURL != dedicated.URL+"/independent/models" {
		t.Fatalf("verified route: %+v, %v", route, err)
	}
	if _, err := client.ListModels(t.Context(), grant, route); errorCode(err) != "model_incompatible" || catalogRequests.Load() != 1 {
		t.Fatalf("empty compatible catalog expected after precisely one lookup: %v", err)
	}

	grant.AccountID = "missing"
	if _, err := client.ResolveRoute(t.Context(), grant); errorCode(err) != "codex_workspace_unverified" {
		t.Fatalf("missing selected workspace accepted: %v", err)
	}
}

func TestRefreshNotSentWhenContextAlreadyCancelled(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := makeClient(t, fakeClient(t, ChannelChatGPT, server.URL))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Refresh(ctx, Grant{ClientID: testIssuedClient, Resource: ChatGPTResource, RefreshToken: "temporary-test-value"})
	if !errors.Is(err, context.Canceled) || requests.Load() != 0 {
		t.Fatalf("cancelled-before-send must preserve the grant: %v", err)
	}
}

func TestAdapterRejectsRedirectsAndForeignCredentials(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	codex := makeClient(t, fakeClient(t, ChannelCodex, origin.URL))
	chatGPT := makeClient(t, fakeClient(t, ChannelChatGPT, origin.URL))
	codexGrant := Grant{ClientID: CodexClientID, Subject: "codex", AccountID: "codex-account", UserID: "codex-user", AccessToken: "test-codex-access", Scopes: []string{directScope}}
	chatGPTGrant := Grant{ClientID: testIssuedClient, Resource: ChatGPTResource, Subject: "chatgpt", AccessToken: "test-plan-access", Scopes: []string{directScope}}
	if _, err := chatGPT.RequestHeaders(codexGrant, Route{}, true); errorCode(err) != "invalid_client" {
		t.Fatalf("Codex credentials accepted by ChatGPT: %v", err)
	}
	if _, err := codex.RequestHeaders(chatGPTGrant, Route{}, false); errorCode(err) != "codex_workspace_unverified" {
		t.Fatalf("ChatGPT credentials accepted by Codex: %v", err)
	}
	_, err := codex.Exchange(t.Context(), "code", "verifier", testRedirect, "", "", Grant{})
	var refusal *UpstreamError
	if !errors.As(err, &refusal) || refusal.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirected exchange: %v", err)
	}
	if _, err := codex.ResolveRoute(t.Context(), codexGrant); !errors.As(err, &refusal) || refusal.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirected account routing: %v", err)
	}
	if foreignRequests.Load() != 0 {
		t.Fatalf("pinned adapter followed a redirect")
	}
}
