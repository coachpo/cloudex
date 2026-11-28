package codexgateway

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/coder/websocket"
)

type transportFixture struct {
	t       *testing.T
	service *Service
	server  *httptest.Server
	db      *store.Store
}

func newTransportFixture(t *testing.T, adapters ...*codexupstream.Client) *transportFixture {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	block, _ := aes.NewCipher(make([]byte, 32))
	key, _ := cipher.NewGCM(block)
	return newTransportFixtureWithDB(t, db, key, adapters...)
}

func newTransportFixtureWithDB(t *testing.T, db *store.Store, key cipher.AEAD, adapters ...*codexupstream.Client) *transportFixture {
	t.Helper()
	service, err := New(t.Context(), Config{Store: db, Key: key, Origin: "https://fixture.invalid", Clients: adapters})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	gateway := httptest.NewServer(service.GatewayHandler())
	t.Cleanup(gateway.Close)
	return &transportFixture{t: t, service: service, server: gateway, db: db}
}

func (f *transportFixture) grant(adapter *codexupstream.Client) store.Grant {
	f.t.Helper()
	channel := adapter.Channel()
	token := codexupstream.Grant{
		ClientID: codexupstream.CodexClientID, Subject: "fixture-user-" + channel,
		UserID: "fixture-user-" + channel, AccountID: "fixture-account-" + channel,
		AccessToken: "access-" + channel, RefreshToken: "refresh-" + channel, ExpiresAt: time.Now().Add(time.Hour),
	}
	if channel == codexupstream.ChannelChatGPT {
		token.ClientID = "fixture-issued"
		token.Resource = codexupstream.ChatGPTResource
		token.Scopes = []string{"chatgpt.tokens.use.direct"}
		token.AccountID = ""
	}
	record, err := f.service.grantRecord(store.ID(), adapter, token)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.AddGrant(f.t.Context(), record); err != nil {
		f.t.Fatal(err)
	}
	record, err = f.db.Grant(f.t.Context(), record.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return record
}

func (f *transportFixture) workspace(name string, grant store.Grant) (store.Workspace, string) {
	f.t.Helper()
	ctx := f.t.Context()
	w := store.Workspace{ID: store.ID(), Name: name, Image: "fixture", Branch: "main", Desired: "running", CPU: 1000, Memory: 1024}
	op, err := f.db.CreateWithGrant(ctx, w, "", "", nil, grant.ID, grant.GrantEpoch)
	if err != nil {
		f.t.Fatal(err)
	}
	running, _, err := f.db.ClaimOp(ctx)
	if err != nil || running.ID != op.ID {
		f.t.Fatalf("claim: %v %v", running, err)
	}
	record := store.Container{ID: store.ID(), Generation: op.Target, ImageID: "fixture", Status: "running"}
	if err := f.db.FinishOp(ctx, running, "running", &record, ""); err != nil {
		f.t.Fatal(err)
	}
	w, err = f.db.Workspace(ctx, w.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return w, f.install(w, record.ID, grant)
}

func (f *transportFixture) install(w store.Workspace, docker string, grant store.Grant) string {
	f.t.Helper()
	ctx := f.t.Context()
	b, err := f.db.Binding(ctx, w.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	token := store.ID()
	pending, err := f.db.IssueCredential(ctx, store.GatewayCredential{
		WorkspaceID: w.ID, HostID: w.HostID, Generation: w.Generation, DockerID: docker,
		StartedAt: "fixture-start", Digest: store.GatewayCredentialDigest(token), GrantID: grant.ID,
		GrantEpoch: grant.GrantEpoch, BindingRevision: b.BindingRevision,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	ws := f.service.workspaceState(w.ID)
	if err := f.service.confirmInstallation(ctx, ws, ws.verification, pending); err != nil {
		f.t.Fatal(err)
	}
	return token
}

func transportAdapter(t *testing.T, channel, base string, client *http.Client) *codexupstream.Client {
	t.Helper()
	config := codexupstream.OpenAICodexConfig()
	if channel == codexupstream.ChannelChatGPT {
		config = codexupstream.ChatGPTConfig()
	}
	config.Issuer = base
	config.AuthorizationURL, config.TokenURL, config.JWKSURL = base+"/authorize", base+"/token", base+"/keys"
	config.RevokeURL, config.DiscoveryURL = base+"/revoke", base+"/.well-known/openid-configuration"
	config.AccountsURL, config.ModelsURL, config.ResponsesURL = base+"/accounts", base+"/models", base+"/responses"
	config.HTTPClient = client
	adapter, err := codexupstream.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestGatewayCodexSettingsRequiresActiveCredentialAndReadsCurrentPair(t *testing.T) {
	// Settings must be available for an admitted credential without consulting an upstream adapter.
	f := newTransportFixture(t)
	adapter := transportAdapter(t, codexupstream.ChannelCodex, "https://fixture.invalid", nil)
	_, token := f.workspace("codex-settings", f.grant(adapter))
	get := func(bearer string, status int) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+GatewayPath+"/codex-settings", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		if res.StatusCode != status || res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("status=%d headers=%v; want %d with uncached JSON", res.StatusCode, res.Header, status)
		}
		return res
	}
	for _, bearer := range []string{"", "invalid-fixture"} {
		res := get(bearer, http.StatusUnauthorized)
		var result struct{ Error struct{ Code string } }
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil || result.Error.Code != "invalid_gateway_credential" {
			t.Fatalf("credential error = %+v (%v)", result.Error, err)
		}
	}
	assertFiles := func(want store.CodexSettings) {
		t.Helper()
		res := get(token, http.StatusOK)
		var got store.CodexSettings
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil || got != want {
			t.Fatalf("gateway settings = %+v (%v); want %+v", got, err, want)
		}
	}
	assertFiles(store.CodexSettings{})
	for _, settings := range []store.CodexSettings{
		{ConfigTOML: "model_context_window = 120000\n", AgentsMD: "# Fixture instructions\n"},
		{ConfigTOML: "model_context_window = 240000\n"},
	} {
		if err := f.db.SetCodexSettings(t.Context(), settings); err != nil {
			t.Fatal(err)
		}
		assertFiles(settings)
	}
}

const transportModel = "gpt-6.2-sol"

func transportCatalog(t *testing.T, channel string, handler http.HandlerFunc) *httptest.Server {
	return transportCatalogServer(t, channel, handler, false)
}

func transportCatalogServer(t *testing.T, channel string, handler http.HandlerFunc, trustedTLS bool) *httptest.Server {
	t.Helper()
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-"+channel {
			t.Errorf("request to %s had wrong upstream identity", r.URL.Path)
		}
		if channel == codexupstream.ChannelCodex && r.Header.Get("ChatGPT-Account-Id") != "fixture-account-codex" {
			t.Errorf("incorrect selected upstream workspace on %s", r.URL.Path)
		}
		if channel == codexupstream.ChannelChatGPT && r.Header.Get("ChatGPT-Account-Id") != "" {
			t.Errorf("public plan request used a Codex account header")
		}
		switch r.URL.Path {
		case "/accounts":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accounts":[{"id":"fixture-account-codex","name":"Fixture account","workspace_backend_origin":"NO_CONSTRAINT","account_routing_override":"NO_CONSTRAINT"}]}`)
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"models":[{"slug":"%s","display_name":"Fixture","visibility":"list","shell_type":"unified_exec","base_instructions":"Fixture instructions","supported_reasoning_levels":[],"experimental_supported_tools":[],"support_verbosity":false,"priority":0,"truncation_policy":{"mode":"tokens","limit":10000}}]}`, transportModel)
		case "/responses":
			handler(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	var upstream *httptest.Server
	if trustedTLS {
		upstream = httptest.NewTLSServer(dispatch)
	} else {
		upstream = httptest.NewServer(dispatch)
	}
	t.Cleanup(upstream.Close)
	return upstream
}

func TestGatewaySSEUpstreamTLSRequiresTrustedCertificate(t *testing.T) {
	upstream := transportCatalogServer(t, "chatgpt", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !r.TLS.HandshakeComplete || r.ProtoMajor != 1 || r.ProtoMinor != 1 {
			t.Errorf("expected TLS HTTP/1.1, got %s %+v", r.Proto, r.TLS)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", transportTerminal)
	}, true)
	adapter := transportAdapter(t, "chatgpt", upstream.URL, upstream.Client())
	f := newTransportFixture(t, adapter)
	_, token := f.workspace("tls", f.grant(adapter))
	if resp, body := f.sse(token, transportInput); resp.StatusCode != 200 || !strings.Contains(body, "response.completed") {
		t.Fatalf("trusted SSE response: %d %s", resp.StatusCode, body)
	}

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialResponsesHTTP(t.Context(), &http.Client{}, target)
	if conn != nil {
		_ = conn.Close()
	}
	var verificationError *tls.CertificateVerificationError
	if !errors.As(err, &verificationError) {
		t.Fatalf("untrusted SSE certificate: %v", err)
	}
}

func transportHeaders(token string) http.Header {
	return http.Header{
		"Authorization":    {"Bearer " + token},
		gatewayRunHeader:   {"test-run"},
		"Thread-Id":        {"test-thread"},
		turnMetadataHeader: {`{"thread_id":"test-thread","turn_id":"test-turn"}`},
	}
}

func (f *transportFixture) sse(token, input string) (*http.Response, string) {
	f.t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"model":"%s","store":false,"stream":true,"input":%s}`, transportModel, input))
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost, f.server.URL+GatewayPath+"/responses", body)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header = transportHeaders(token)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	output, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp, string(output)
}

func (f *transportFixture) ws(ctx context.Context, token string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, f.server.URL+GatewayPath+"/responses", &websocket.DialOptions{
		HTTPClient: f.server.Client(), HTTPHeader: transportHeaders(token),
	})
}

func transportGenerate(callResult string) []byte {
	return fmt.Appendf(nil, `{"type":"response.create","model":"%s","stream":true,"store":false,"input":%s}`, transportModel, callResult)
}

const transportInput = `[{"role":"user","type":"message","content":"fixture input"}]`
const transportOrphan = `[{"type":"function_call_output","call_id":"call_from_a","output":"old tool result"}]`
const transportTerminal = `{"type":"response.completed","response":{"id":"resp_fixture","usage":{"input_tokens":11,"output_tokens":9}}}`

func TestGatewayUnknownSubmissionSurvivesRestartAndSSEFallback(t *testing.T) {
	for _, channel := range []string{codexupstream.ChannelCodex, codexupstream.ChannelChatGPT} {
		t.Run(channel, func(t *testing.T) {
			var written atomic.Int32
			upstream := transportCatalog(t, channel, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					written.Add(1)
					t.Error("automatic fallback reached the upstream after a possibly accepted generation")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err == nil {
					written.Add(1)
				}
			})
			adapter := transportAdapter(t, channel, upstream.URL, upstream.Client())
			f := newTransportFixture(t, adapter)
			record := f.grant(adapter)
			w, token := f.workspace("restart", record)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			conn, resp, err := f.ws(ctx, token)
			if err != nil {
				t.Fatalf("upgrade: %v %+v", err, resp)
			}
			if err := conn.Write(ctx, websocket.MessageText, transportGenerate(transportInput)); err != nil {
				t.Fatal(err)
			}
			_, frame, err := conn.Read(ctx)
			_ = conn.CloseNow()
			if err != nil || !strings.Contains(string(frame), "submission_outcome_unknown") || written.Load() != 1 {
				t.Fatalf("expected unknown outcome after one write: %q %v count=%d", frame, err, written.Load())
			}
			if retry, body := f.sse(token, transportInput); retry.StatusCode != 409 || !strings.Contains(body, "submission_outcome_unknown") {
				t.Fatalf("retry response: %d %s", retry.StatusCode, body)
			}

			f.service.Close()
			afterCrash := newTransportFixtureWithDB(t, f.db, f.service.key, adapter)
			previous, err := f.db.WorkspaceCredential(t.Context(), w.ID)
			if err != nil {
				t.Fatal(err)
			}
			state := afterCrash.service.workspaceState(w.ID)
			if err := afterCrash.service.confirmInstallation(t.Context(), state, state.verification, previous); err != nil {
				t.Fatal(err)
			}
			if retry, body := afterCrash.sse(token, transportInput); retry.StatusCode != 409 || !strings.Contains(body, "submission_outcome_unknown") || written.Load() != 1 {
				t.Fatalf("retry after restart: %d %s writes=%d", retry.StatusCode, body, written.Load())
			}
		})
	}
}

type fixtureTrip func(r *http.Request) (*http.Response, error)

func (f fixtureTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayRebindPreventsLateHandshakeBothDirections(t *testing.T) {
	for _, pair := range [][2]string{{"codex", "chatgpt"}, {"chatgpt", "codex"}} {
		for _, stage := range []string{"before-dial", "after-upstream-101"} {
			t.Run(pair[0]+"-to-"+pair[1]+"/"+stage, func(t *testing.T) {
				paused := make(chan struct{})
				release := make(chan struct{})
				var pause atomic.Bool
				pause.Store(true)
				var oldGenerations, newGenerations, oldUpgrades atomic.Int32
				old := transportCatalog(t, pair[0], func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						body, _ := io.ReadAll(r.Body)
						if strings.Contains(string(body), "old tool result") {
							t.Error("orphan tool result reached A")
						}
						oldGenerations.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", transportTerminal)
						return
					}
					oldUpgrades.Add(1)
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
					if _, _, err := conn.Read(r.Context()); err == nil {
						oldGenerations.Add(1)
						t.Error("a late W1 handshake reached generation")
					}
				})
				original := old.Client().Transport
				oldClient := &http.Client{Transport: fixtureTrip(func(r *http.Request) (*http.Response, error) {
					if stage == "before-dial" && r.URL.Path == "/responses" && r.Method == http.MethodGet && pause.Swap(false) {
						close(paused)
						select {
						case <-release:
						case <-t.Context().Done():
						}
						if r.Context().Err() != nil {
							return nil, r.Context().Err()
						}
					}
					response, err := original.RoundTrip(r)
					if stage == "after-upstream-101" && err == nil && response.StatusCode == 101 && pause.Swap(false) {
						close(paused)
						select {
						case <-release:
						case <-t.Context().Done():
						}
					}
					return response, err
				})}
				newUpstream := transportCatalog(t, pair[1], func(w http.ResponseWriter, r *http.Request) {
					newGenerations.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", transportTerminal)
				})
				adapterA := transportAdapter(t, pair[0], old.URL, oldClient)
				adapterB := transportAdapter(t, pair[1], newUpstream.URL, newUpstream.Client())
				f := newTransportFixture(t, adapterA, adapterB)
				a := f.grant(adapterA)
				w1, token1 := f.workspace("one", a)
				w2, token2 := f.workspace("two", a)
				b := f.grant(adapterB)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				var lateStatus atomic.Int32
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					conn, resp, err := f.ws(ctx, token1)
					if resp != nil {
						lateStatus.Store(int32(resp.StatusCode))
					}
					if err == nil {
						_ = conn.CloseNow()
					}
				}()
				select {
				case <-paused:
				case <-ctx.Done():
					t.Fatal("handshake did not reach the pause point")
				}
				binding, err := f.service.SetBinding(ctx, w1.ID, b.ID, 1)
				close(release)
				if err != nil || binding.BindingRevision != 2 {
					t.Fatalf("rebind: %v, %v", binding, err)
				}
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("late connection did not close")
				}
				if lateStatus.Load() != 401 {
					t.Fatalf("stale upstream handshake acknowledged downstream: HTTP %d", lateStatus.Load())
				}
				docker, err := f.db.Container(t.Context(), w1.ID, w1.Generation)
				if err != nil {
					t.Fatal(err)
				}
				tokenB := f.install(w1, docker.ID, b)
				if response, body := f.sse(tokenB, transportOrphan); response.StatusCode != 409 || !strings.Contains(body, "foreign_response_context") {
					t.Fatalf("cross-binding call accepted: %d %s", response.StatusCode, body)
				}
				if response, body := f.sse(tokenB, transportInput); response.StatusCode != 200 || !strings.Contains(body, "response.completed") {
					t.Fatalf("W1 bound to B: %d %s", response.StatusCode, body)
				}
				if response, body := f.sse(token2, transportInput); response.StatusCode != 200 || !strings.Contains(body, "response.completed") {
					t.Fatalf("W2 using A: %d %s", response.StatusCode, body)
				}
				if response, _ := f.sse(token1, transportInput); response.StatusCode != 401 {
					t.Fatalf("old W1 credential survived rebind: %d", response.StatusCode)
				}
				wantUpgrades := int32(0)
				if stage == "after-upstream-101" {
					wantUpgrades = 1
				}
				if oldUpgrades.Load() != wantUpgrades {
					t.Fatalf("obsolete upstream upgrades=%d, want %d", oldUpgrades.Load(), wantUpgrades)
				}
				if oldGenerations.Load() != 1 || newGenerations.Load() != 1 {
					t.Fatalf("generation counts after rebind A=%d B=%d workspace=%s", oldGenerations.Load(), newGenerations.Load(), w2.ID)
				}
			})
		}
	}
}

func TestGatewayLimitPreservesRetryAndRedactsUpstreamCredential(t *testing.T) {
	upstream := transportCatalog(t, "chatgpt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.Header().Set("X-Request-Id", "req_fixture")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"detail":{"code":"subscription_sharing_usage_limit_exceeded","message":"credential access-chatgpt"}}`)
	})
	adapter := transportAdapter(t, "chatgpt", upstream.URL, upstream.Client())
	f := newTransportFixture(t, adapter)
	_, token := f.workspace("limited", f.grant(adapter))
	resp, body := f.sse(token, transportInput)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "60" || resp.Header.Get("X-Request-Id") != "req_fixture" ||
		strings.Contains(body, "access-chatgpt") || !strings.Contains(body, "subscription_sharing_usage_limit_exceeded") {
		t.Fatalf("misclassified 429, status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
	}
}

func TestGatewaySSEFrameBoundaries(t *testing.T) {
	wire := "event: response.completed\r\ndata: {\"response\":\r\ndata: {\"id\": \"resp_fixture\"}}\r\n\r\n"
	var progress responseProgress
	err := consumeSSE(bufioScanner(wire), func([]byte) {}, func(event sseEvent) error {
		progress.observe(event.data, event.kind)
		return nil
	}, func() bool { return progress.terminal })
	if err != nil || progress.id != "resp_fixture" || progress.kind != "response.completed" {
		t.Fatalf("multi-line event was not read: %+v %v", progress, err)
	}
	partial := strings.TrimSuffix(wire, "\r\n")
	progress = responseProgress{}
	err = consumeSSE(bufioScanner(partial), func([]byte) {}, func(event sseEvent) error {
		progress.observe(event.data, event.kind)
		return nil
	}, func() bool { return progress.terminal })
	if !errors.Is(err, io.ErrUnexpectedEOF) || progress.terminal {
		t.Fatalf("unfinished SSE event accepted: %+v %v", progress, err)
	}
}

func bufioScanner(body string) *bufio.Scanner { return bufio.NewScanner(strings.NewReader(body)) }

type droppedTerminalWriter struct {
	http.ResponseWriter
	dropped *atomic.Bool
}

func (w droppedTerminalWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w droppedTerminalWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "response.completed") {
		w.dropped.Store(true)
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}

func TestGatewayClientCannotReplayAfterLosingTerminal(t *testing.T) {
	var generations atomic.Int32
	upstream := transportCatalog(t, "chatgpt", func(w http.ResponseWriter, r *http.Request) {
		generations.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_fixture"}}`+"\n\n")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", transportTerminal)
	})
	adapter := transportAdapter(t, "chatgpt", upstream.URL, upstream.Client())
	f := newTransportFixture(t, adapter)
	_, token := f.workspace("loss", f.grant(adapter))
	var dropped atomic.Bool
	handler := f.service.GatewayHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(droppedTerminalWriter{ResponseWriter: w, dropped: &dropped}, r)
	}))
	defer server.Close()
	f.server = server
	resp, body := f.sse(token, transportInput)
	if resp.StatusCode != 200 || !strings.Contains(body, "response.created") || strings.Contains(body, "response.completed") || !dropped.Load() {
		t.Fatalf("terminal loss fault didn't exercise downlink: status=%d body=%s dropped=%v", resp.StatusCode, body, dropped.Load())
	}
	resp, body = f.sse(token, transportInput)
	if resp.StatusCode != 409 || !strings.Contains(body, "submission_outcome_unknown") || generations.Load() != 1 {
		t.Fatalf("retry after dropped terminal: status=%d body=%s generations=%d", resp.StatusCode, body, generations.Load())
	}
}

func TestGatewayCompactionAllowsOneContinuationInTheNextWindow(t *testing.T) {
	var generations atomic.Int32
	upstream := transportCatalog(t, "chatgpt", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := generations.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_%d\"}}\n\n", n)
		if !strings.Contains(string(body), "lose terminal") {
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\"}}\n\n", n)
		}
	})
	adapter := transportAdapter(t, "chatgpt", upstream.URL, upstream.Client())
	f := newTransportFixture(t, adapter)
	_, token := f.workspace("compaction", f.grant(adapter))
	send := func(turn, kind string, window int, input string) (int, string) {
		t.Helper()
		metadata, _ := json.Marshal(map[string]string{
			"x-codex-turn-metadata": fmt.Sprintf(`{"turn_id":%q,"request_kind":%q,"window_number":%d}`, turn, kind, window),
		})
		body := fmt.Sprintf(`{"model":%q,"store":false,"stream":true,"input":[{"role":"user","type":"message","content":%q}],"client_metadata":%s}`,
			transportModel, input, metadata)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+GatewayPath+"/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = transportHeaders(token)
		req.Header.Set(turnMetadataHeader, fmt.Sprintf(`{"thread_id":"test-thread","turn_id":%q}`, turn))
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		output, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(output)
	}
	expect := func(step string, status int, body string, completed bool, total int32) {
		t.Helper()
		if completed && (status != 200 || !strings.Contains(body, "response.completed")) ||
			!completed && (status != 409 || !strings.Contains(body, "submission_outcome_unknown")) || generations.Load() != total {
			t.Fatalf("%s: status=%d generations=%d body=%s", step, status, generations.Load(), body)
		}
	}

	status, body := send("compacted", "compaction", 0, "summarize")
	expect("compaction", status, body, true, 1)
	status, body = send("compacted", "turn", 1, "continue with the summary")
	expect("continuation in the next window", status, body, true, 2)
	status, body = send("compacted", "turn", 1, "continue with the summary")
	expect("replayed continuation", status, body, false, 2)

	status, body = send("replayed", "compaction", 0, "summarize")
	expect("second turn compaction", status, body, true, 3)
	status, body = send("replayed", "compaction", 0, "summarize")
	expect("replayed compaction", status, body, false, 3)

	if status, body = send("lost", "compaction", 0, "summarize and lose terminal"); status != 200 || generations.Load() != 4 {
		t.Fatalf("compaction without terminal: status=%d generations=%d body=%s", status, generations.Load(), body)
	}
	status, body = send("lost", "turn", 1, "continue with the summary")
	expect("continuation after a compaction without terminal", status, body, false, 4)
}

type observedWriteConn struct {
	net.Conn
	write func([]byte) (int, error)
}

func (c observedWriteConn) Write(data []byte) (int, error) { return c.write(data) }

type responseResult struct {
	response *http.Response
	err      error
}

func TestGatewayRebindCancelsOldSSEAndRejectsOldToolResult(t *testing.T) {
	for _, pair := range [][2]string{{"codex", "chatgpt"}, {"chatgpt", "codex"}} {
		t.Run(pair[0]+"-to-"+pair[1], func(t *testing.T) {
			var acceptedA, acceptedB atomic.Int32
			started := make(chan struct{})
			canceled := make(chan struct{})
			beforeWrite := make(chan struct{})
			allowWrite := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			old := transportCatalog(t, pair[0], func(w http.ResponseWriter, r *http.Request) {
				acceptedA.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), "stream in flight") {
					_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_in_flight"}}`+"\n\n")
					w.(http.Flusher).Flush()
					close(started)
					select {
					case <-r.Context().Done():
						close(canceled)
					case <-release:
					}
					return
				}
				_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_from_a","name":"test","arguments":"{}"}}`+"\n\n")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", transportTerminal)
			})
			next := transportCatalog(t, pair[1], func(w http.ResponseWriter, r *http.Request) {
				acceptedB.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", transportTerminal)
			})
			oldTransport := old.Client().Transport.(*http.Transport).Clone()
			var dialer net.Dialer
			oldTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return observedWriteConn{Conn: conn, write: func(data []byte) (int, error) {
					if !strings.Contains(string(data), "stream in flight") {
						return conn.Write(data)
					}
					close(beforeWrite)
					select {
					case <-allowWrite:
					case <-t.Context().Done():
						return 0, t.Context().Err()
					}
					n, err := conn.Write(data)
					if err == nil {
						select {
						case <-started:
						case <-t.Context().Done():
							return n, t.Context().Err()
						}
					}
					return n, err
				}}, nil
			}
			oldClient := &http.Client{Transport: oldTransport}
			adapterA := transportAdapter(t, pair[0], old.URL, oldClient)
			adapterB := transportAdapter(t, pair[1], next.URL, next.Client())
			f := newTransportFixture(t, adapterA, adapterB)
			a := f.grant(adapterA)
			w1, token1 := f.workspace("one", a)
			_, token2 := f.workspace("two", a)
			b := f.grant(adapterB)
			if resp, body := f.sse(token1, transportInput); resp.StatusCode != 200 ||
				!strings.Contains(body, "call_from_a") || !strings.Contains(body, "response.completed") {
				t.Fatalf("old tool fixture: %s %s", resp.Status, body)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			streaming := strings.NewReader(fmt.Sprintf(`{"model":"%s","input":[{"role":"user","type":"message","content":"stream in flight"}]}`, transportModel))
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+GatewayPath+"/responses", streaming)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = transportHeaders(token1)
			request.Header.Set(turnMetadataHeader, `{"thread_id":"test-thread","turn_id":"in-flight-turn"}`)
			inflight := make(chan responseResult, 1)
			go func() {
				response, err := f.server.Client().Do(request)
				inflight <- responseResult{response: response, err: err}
			}()
			select {
			case <-beforeWrite:
			case <-ctx.Done():
				t.Fatal("request did not reach the write gate")
			}
			if state := f.service.workspaceState(w1.ID); state.fence.TryLock() {
				state.fence.Unlock()
				close(allowWrite)
				t.Fatal("the write boundary did not hold the same fence as rebind")
			}
			switched := make(chan error, 1)
			go func() {
				binding, err := f.service.SetBinding(ctx, w1.ID, b.ID, 1)
				if err == nil && binding.BindingRevision != 2 {
					err = fmt.Errorf("unexpected revision after switch: %d", binding.BindingRevision)
				}
				switched <- err
			}()
			close(allowWrite)
			select {
			case err := <-switched:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("rebind did not commit after the admitted write")
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("old upstream SSE was not canceled")
			}
			stream := <-inflight
			if stream.response != nil {
				body, _ := io.ReadAll(stream.response.Body)
				_ = stream.response.Body.Close()
				if strings.Contains(string(body), "response.completed") {
					t.Fatalf("late SSE terminal crossed binding revision: %s", body)
				}
			}
			instance, err := f.db.Container(ctx, w1.ID, w1.Generation)
			if err != nil {
				t.Fatal(err)
			}
			tokenB := f.install(w1, instance.ID, b)
			if response, body := f.sse(tokenB, transportOrphan); response.StatusCode != 409 || !strings.Contains(body, "foreign_response_context") {
				t.Fatalf("accepted old A call_id on B: %d %s", response.StatusCode, body)
			}
			if response, _ := f.sse(token1, transportOrphan); response.StatusCode != 401 {
				t.Fatalf("old W1 credential accepted: %d", response.StatusCode)
			}
			if response, _ := f.sse(token2, transportInput); response.StatusCode != 200 {
				t.Fatalf("W2 binding changed: %d", response.StatusCode)
			}
			if acceptedA.Load() != 3 || acceptedB.Load() != 0 {
				t.Fatalf("expected three legitimate calls only to A, got A=%d B=%d", acceptedA.Load(), acceptedB.Load())
			}
		})
	}
}

func TestGatewayInterruptHonorsChannelCapability(t *testing.T) {
	for _, channel := range []string{"codex", "chatgpt"} {
		t.Run(channel, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var interrupts atomic.Int32
			upstream := transportCatalog(t, channel, func(w http.ResponseWriter, r *http.Request) {
				socket, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer socket.CloseNow()
				if _, _, err := socket.Read(ctx); err != nil {
					return
				}
				_ = socket.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_interrupted"}}`))
				if _, request, err := socket.Read(ctx); err == nil {
					if !strings.Contains(string(request), `"response_id":"resp_interrupted"`) {
						t.Errorf("interrupt didn't refer to actual response: %s", request)
					}
					interrupts.Add(1)
					_ = socket.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt.accepted","response_id":"resp_interrupted"}`))
					_ = socket.Write(ctx, websocket.MessageText, []byte(`{"type":"response.incomplete","response":{"id":"resp_interrupted","incomplete_details":{"reason":"interrupted"}}}`))
				}
			})
			adapter := transportAdapter(t, channel, upstream.URL, upstream.Client())
			f := newTransportFixture(t, adapter)
			_, token := f.workspace("interrupt", f.grant(adapter))
			socket, _, err := f.ws(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.CloseNow()
			if err := socket.Write(ctx, websocket.MessageText, transportGenerate(transportInput)); err != nil {
				t.Fatal(err)
			}
			_, created, err := socket.Read(ctx)
			if err != nil || !strings.Contains(string(created), "response.created") {
				t.Fatalf("wait for response that can be interrupted: %q %v", created, err)
			}
			if err := socket.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt","response_id":"resp_interrupted"}`)); err != nil {
				t.Fatal(err)
			}
			_, acknowledgement, err := socket.Read(ctx)
			if channel == "chatgpt" {
				if err != nil || !strings.Contains(string(acknowledgement), "interruption_outcome_unknown") || interrupts.Load() != 0 {
					t.Fatalf("unsupported interrupt result: %q %v writes=%d", acknowledgement, err, interrupts.Load())
				}
				return
			}
			if err != nil || !strings.Contains(string(acknowledgement), "response.interrupt.accepted") {
				t.Fatalf("interrupt confirmation: %q %v", acknowledgement, err)
			}
			_, terminal, err := socket.Read(ctx)
			if err != nil || !strings.Contains(string(terminal), "response.incomplete") || interrupts.Load() != 1 {
				t.Fatalf("interrupt terminal: %q %v writes=%d", terminal, err, interrupts.Load())
			}
		})
	}
}

func TestGatewayRebindFencesOldSocketToolResult(t *testing.T) {
	for _, pair := range [][2]string{{"codex", "chatgpt"}, {"chatgpt", "codex"}} {
		t.Run(pair[0]+"-to-"+pair[1], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var opened, acceptedOnW1, acceptedOnW2 atomic.Int32
			done := make(chan struct{}, 2)
			old := transportCatalog(t, pair[0], func(w http.ResponseWriter, r *http.Request) {
				index := opened.Add(1)
				socket, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { done <- struct{}{} }()
				defer socket.CloseNow()
				if _, _, err := socket.Read(ctx); err != nil {
					return
				}
				_ = socket.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_from_a","name":"test","arguments":"{}"}}`))
				_ = socket.Write(ctx, websocket.MessageText, []byte(transportTerminal))
				if _, request, err := socket.Read(ctx); err == nil && strings.Contains(string(request), "old tool result") {
					if index == 1 {
						acceptedOnW1.Add(1)
					} else {
						acceptedOnW2.Add(1)
					}
					_ = socket.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_continued"}}`))
				}
			})
			next := transportCatalog(t, pair[1], func(w http.ResponseWriter, _ *http.Request) {
				t.Error("old WS tool result reached B")
				w.WriteHeader(500)
			})
			adapterA := transportAdapter(t, pair[0], old.URL, old.Client())
			adapterB := transportAdapter(t, pair[1], next.URL, next.Client())
			f := newTransportFixture(t, adapterA, adapterB)
			a := f.grant(adapterA)
			w1, token1 := f.workspace("one", a)
			_, token2 := f.workspace("two", a)
			b := f.grant(adapterB)
			sockets := make([]*websocket.Conn, 0, 2)
			for _, token := range []string{token1, token2} {
				socket, _, err := f.ws(ctx, token)
				if err != nil {
					t.Fatal(err)
				}
				defer socket.CloseNow()
				sockets = append(sockets, socket)
				if err := socket.Write(ctx, websocket.MessageText, transportGenerate(transportInput)); err != nil {
					t.Fatal(err)
				}
				if _, tool, err := socket.Read(ctx); err != nil || !strings.Contains(string(tool), "call_from_a") {
					t.Fatalf("initial tool call: %s %v", tool, err)
				}
				if _, terminal, err := socket.Read(ctx); err != nil || !strings.Contains(string(terminal), "response.completed") {
					t.Fatalf("initial terminal: %s %v", terminal, err)
				}
			}
			if _, err := f.service.SetBinding(ctx, w1.ID, b.ID, 1); err != nil {
				t.Fatal(err)
			}
			_ = sockets[0].Write(ctx, websocket.MessageText, transportGenerate(transportOrphan))
			if err := sockets[1].Write(ctx, websocket.MessageText, transportGenerate(transportOrphan)); err != nil {
				t.Fatal(err)
			}
			if _, terminal, err := sockets[1].Read(ctx); err != nil || !strings.Contains(string(terminal), "resp_continued") {
				t.Fatalf("W2 should continue on A: %s %v", terminal, err)
			}
			for range 2 {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("old upstream connections did not stop")
				}
			}
			if acceptedOnW1.Load() != 0 || acceptedOnW2.Load() != 1 {
				t.Fatalf("actual continuation counts W1=%d W2=%d", acceptedOnW1.Load(), acceptedOnW2.Load())
			}
		})
	}
}

func TestGatewayRebindRestoresCompletedToolHistory(t *testing.T) {
	const (
		callFields    = `"type":"custom_tool_call","call_id":"call_completed_on_a","name":"test","input":"tool input","status":"completed"`
		call          = `{` + callFields + `}`
		result        = `{"type":"custom_tool_call_output","call_id":"call_completed_on_a","output":[{"type":"input_text","text":"tool result"}]}`
		reasoning     = `{"type":"reasoning","encrypted_content":"reasoning_from_a","summary":[]}`
		provisional   = `{"type":"response.in_progress","response":{"id":"resp_fixture","output":[{"type":"reasoning","encrypted_content":"provisional_reasoning","summary":[]},{"type":"custom_tool_call","call_id":"call_completed_on_a","name":"test","input":"draft","status":"in_progress"}]}}`
		pending       = `{"type":"function_call","call_id":"call_pending_on_a","name":"test","arguments":"{}"}`
		pendingResult = `{"type":"function_call_output","call_id":"call_pending_on_a","output":"late result"}`
		resultInput   = `[` + result + `]`
		finishInput   = `[` + reasoning + `,` + call + `,` + result + `]`
		pendingInput  = `[` + pending + `,` + pendingResult + `]`
		invalidEnd    = `{"type":"response.completed","response":{}}`
		resumeInput   = `[` + reasoning + `,` + call + `,` + result + `,{"type":"message","role":"user","content":"continue after rebinding"}]`
		resumed       = `{"type":"response.completed","response":{"id":"resp_after_rebind"}}`
	)
	for _, mode := range []string{"sse", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var resumedOnA atomic.Int32
			upstream := func(channel string) *httptest.Server {
				t.Helper()
				events := func(body []byte) []string {
					var request struct{ Input json.RawMessage }
					if err := json.Unmarshal(body, &request); err != nil {
						t.Errorf("invalid upstream request: %v", err)
						return nil
					}
					if channel == codexupstream.ChannelCodex {
						switch string(request.Input) {
						case transportInput:
							return []string{
								provisional,
								`{"type":"response.output_item.done","item":` + reasoning + `}`,
								`{"type":"response.output_item.done","item":{"id":"ctc_a","namespace":null,"metadata":{"source":"upstream"},"internal_chat_message_metadata_passthrough":{"content_token_count":10},"encrypted_content":"envelope_from_a",` + callFields + `}}`,
								transportTerminal,
							}
						case finishInput, resultInput:
							return []string{`{"type":"response.output_item.done","item":` + pending + `}`, `{"type":"response.completed","response":{"id":"resp_pending"}}`}
						case pendingInput:
							return []string{`{"type":"response.created","response":{"id":"resp_pending_result"}}`, invalidEnd}
						case resumeInput:
							resumedOnA.Add(1)
							return []string{resumed}
						}
					}
					t.Errorf("unexpected generation on %s: %s", channel, request.Input)
					return nil
				}
				return transportCatalog(t, channel, func(w http.ResponseWriter, r *http.Request) {
					if mode == "sse" {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						for _, event := range events(body) {
							_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
						}
						return
					}
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					for {
						_, body, err := conn.Read(ctx)
						if err != nil {
							return
						}
						reply := events(body)
						for _, event := range reply {
							if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
								t.Error(err)
								return
							}
						}
						if len(reply) > 0 && reply[len(reply)-1] == invalidEnd {
							return
						}
					}
				})
			}
			upstreamA, upstreamB := upstream(codexupstream.ChannelCodex), upstream(codexupstream.ChannelChatGPT)
			adapterA := transportAdapter(t, codexupstream.ChannelCodex, upstreamA.URL, upstreamA.Client())
			adapterB := transportAdapter(t, codexupstream.ChannelChatGPT, upstreamB.URL, upstreamB.Client())
			f := newTransportFixture(t, adapterA, adapterB)
			a, b := f.grant(adapterA), f.grant(adapterB)
			w, firstTokenA := f.workspace("switching", a)
			_, otherWorkspaceToken := f.workspace("separate", a)
			instance, err := f.db.Container(ctx, w.ID, w.Generation)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := f.server.URL + GatewayPath + "/responses"
			open := func(token, thread, run, turn string) func(string, int, string) {
				t.Helper()
				headers := transportHeaders(token)
				headers.Set(gatewayRunHeader, run)
				headers.Set("Thread-Id", thread)
				headers.Set(turnMetadataHeader, fmt.Sprintf(`{"thread_id":%q,"turn_id":%q}`, thread, turn))
				if mode == "sse" {
					return func(input string, wantStatus int, marker string) {
						t.Helper()
						request := fmt.Sprintf(`{"model":%q,"stream":true,"input":%s}`, transportModel, input)
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(request))
						if err != nil {
							t.Fatal(err)
						}
						req.Header = headers
						resp, err := f.server.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						body, err := io.ReadAll(resp.Body)
						if err != nil || resp.StatusCode != wantStatus || !strings.Contains(string(body), marker) ||
							(wantStatus == http.StatusOK && !strings.Contains(string(body), "response.completed")) {
							t.Fatalf("SSE status=%d body=%s err=%v, want %d and %q", resp.StatusCode, body, err, wantStatus, marker)
						}
					}
				}
				conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: f.server.Client(), HTTPHeader: headers})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.CloseNow() })
				return func(input string, wantStatus int, marker string) {
					t.Helper()
					body := transportGenerate(input)
					if input == resultInput && wantStatus == http.StatusOK {
						body = append(body[:len(body)-1], `,"previous_response_id":"resp_fixture"}`...)
					}
					if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
						t.Fatal(err)
					}
					var received strings.Builder
					for {
						_, frame, err := conn.Read(ctx)
						if err != nil {
							t.Fatalf("read WebSocket after %s: %v", received.String(), err)
						}
						received.Write(frame)
						var event struct {
							Type   string
							Status int
						}
						if err := json.Unmarshal(frame, &event); err != nil {
							t.Fatal(err)
						}
						switch event.Type {
						case "error":
							if event.Status != wantStatus || !strings.Contains(string(frame), marker) {
								t.Fatalf("WebSocket error %s, want %d and %q", frame, wantStatus, marker)
							}
							return
						case "response.completed":
							if wantStatus != http.StatusOK || !strings.Contains(received.String(), marker) {
								t.Fatalf("WebSocket completed with %s, want %d and %q", received.String(), wantStatus, marker)
							}
							return
						}
					}
				}
			}
			rebind := func(grant store.Grant, revision int64) string {
				t.Helper()
				binding, err := f.service.SetBinding(ctx, w.ID, grant.ID, revision)
				if err != nil || binding.GrantID != grant.ID || binding.BindingRevision != revision+1 {
					t.Fatalf("rebind: %+v %v", binding, err)
				}
				return f.install(w, instance.ID, grant)
			}

			const thread = "test-thread"
			firstA := open(firstTokenA, thread, "original-run", "original-turn")
			firstA(transportInput, http.StatusOK, "call_completed_on_a")
			if mode == "websocket" {
				firstA(resultInput, http.StatusOK, "call_pending_on_a")
				firstA(pendingInput, http.StatusConflict, "submission_outcome_unknown")
			} else {
				firstA(finishInput, http.StatusOK, "call_pending_on_a")
			}
			tokenB := rebind(b, 1)
			open(tokenB, thread, "b-run", "b-turn")(resumeInput, http.StatusConflict, "foreign_response_context")
			tokenA := rebind(a, 2)
			if mode == "websocket" {
				stale, response, err := f.ws(ctx, firstTokenA)
				if stale != nil {
					_ = stale.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("first A credential upgraded after rebinding: response=%v err=%v", response, err)
				}
			} else if response, body := f.sse(firstTokenA, resumeInput); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("first A credential after rebinding: %d %s", response.StatusCode, body)
			}
			open(otherWorkspaceToken, thread, "other-run", "other-turn")(resumeInput, http.StatusConflict, "foreign_response_context")
			open(tokenA, "other-thread", "resumed-run", "resumed-turn")(resumeInput, http.StatusConflict, "foreign_response_context")
			for _, input := range []string{
				resultInput,
				pendingInput,
				strings.Replace(resumeInput, "tool result", "altered result", 1),
				strings.Replace(resumeInput, "tool input", "altered input", 1),
				strings.Replace(resumeInput, `"name":"test"`, `"name":"different_tool"`, 1),
				strings.Replace(resumeInput, "reasoning_from_a", "unknown_reasoning", 1),
				strings.Replace(resumeInput, "reasoning_from_a", "provisional_reasoning", 1),
				strings.Replace(resumeInput, call, `{"encrypted_content":"unverified_envelope",`+callFields+`}`, 1),
			} {
				open(tokenA, thread, "resumed-run", "resumed-turn")(input, http.StatusConflict, "foreign_response_context")
			}
			open(tokenA, thread, "resumed-run", "resumed-turn")(resumeInput, http.StatusOK, "resp_after_rebind")
			if resumedOnA.Load() != 1 {
				t.Fatalf("resumed generations on A = %d, want 1", resumedOnA.Load())
			}
			if err := f.db.ReplaceGrant(ctx, a, a.GrantEpoch); err != nil {
				t.Fatal(err)
			}
			reauthorized, err := f.db.Grant(ctx, a.ID)
			if err != nil || reauthorized.GrantEpoch != a.GrantEpoch+1 {
				t.Fatalf("reauthorized grant epoch: %d, %v", reauthorized.GrantEpoch, err)
			}
			tokenNextEpoch := f.install(w, instance.ID, reauthorized)
			open(tokenNextEpoch, thread, "reauthorized-run", "reauthorized-turn")(resumeInput, http.StatusConflict, "foreign_response_context")
		})
	}
}

func TestGatewayRedactsCredentialFromStreamingEvents(t *testing.T) {
	const delta = `{"type":"response.output_text.delta","delta":"credential access-chatgpt"}`
	upstream := transportCatalog(t, "chatgpt", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\ndata: %s\n\n", delta, transportTerminal)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept fixture WebSocket: %v", err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			t.Errorf("read fixture generation: %v", err)
			return
		}
		for _, event := range []string{delta, transportTerminal} {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
				t.Errorf("write fixture event: %v", err)
				return
			}
		}
	})
	adapter := transportAdapter(t, "chatgpt", upstream.URL, upstream.Client())
	f := newTransportFixture(t, adapter)
	grant := f.grant(adapter)
	t.Run("sse", func(t *testing.T) {
		_, token := f.workspace("redaction-sse", grant)
		response, output := f.sse(token, transportInput)
		if response.StatusCode != http.StatusOK || strings.Contains(output, "access-chatgpt") ||
			!strings.Contains(output, "event: response.output_text.delta\n") || !strings.Contains(output, "credential [redacted]") ||
			!strings.Contains(output, transportTerminal) {
			t.Fatal("SSE did not preserve the event and terminal while redacting the upstream credential")
		}
	})
	t.Run("ws", func(t *testing.T) {
		_, token := f.workspace("redaction-ws", grant)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, _, err := f.ws(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		if err := conn.Write(ctx, websocket.MessageText, transportGenerate(transportInput)); err != nil {
			t.Fatal(err)
		}
		_, redacted, err := conn.Read(ctx)
		if err != nil || strings.Contains(string(redacted), "access-chatgpt") || !strings.Contains(string(redacted), "credential [redacted]") {
			t.Fatal("WebSocket forwarded an unredacted streaming event")
		}
		_, terminal, err := conn.Read(ctx)
		if err != nil || string(terminal) != transportTerminal {
			t.Fatalf("WebSocket terminal was not preserved: %v", err)
		}
	})
}

func TestGatewayWebSocketContinuationStaysInItsThread(t *testing.T) {
	var submitted atomic.Int32
	upstream := transportCatalog(t, "codex", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept fixture WebSocket: %v", err)
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			submitted.Add(1)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(transportTerminal)); err != nil {
				return
			}
		}
	})
	adapter := transportAdapter(t, "codex", upstream.URL, upstream.Client())
	f := newTransportFixture(t, adapter)
	_, token := f.workspace("thread-continuation", f.grant(adapter))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	headers := transportHeaders(token)
	headers.Del("Thread-Id")
	headers.Del(turnMetadataHeader)
	conn, _, err := websocket.Dial(ctx, f.server.URL+GatewayPath+"/responses", &websocket.DialOptions{
		HTTPClient: f.server.Client(), HTTPHeader: headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	send := func(thread, previous string) []byte {
		t.Helper()
		request := fmt.Appendf(nil, `{"type":"response.create","model":"%s","stream":true,"store":false,"input":%s,"client_metadata":{"thread_id":%q},"previous_response_id":%q}`,
			transportModel, transportInput, thread, previous)
		if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		_, result, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if first := send("thread-a", ""); string(first) != transportTerminal {
		t.Fatalf("initial generation did not complete: %s", first)
	}
	if alternate := send("thread-b", "resp_fixture"); !strings.Contains(string(alternate), "previous_response_not_found") || submitted.Load() != 1 {
		t.Fatalf("another thread continued the response, upstream submissions=%d", submitted.Load())
	}
}

func TestGatewayUpgradeRefusalClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		channel    string
		upstream   int
		code       string
		downstream int
		streaming  bool
	}{
		{"method-not-upgradable", "chatgpt", http.StatusMethodNotAllowed, "", http.StatusUpgradeRequired, false},
		{"success-without-upgrade", "chatgpt", http.StatusOK, "", http.StatusUpgradeRequired, true},
		{"invalid-upgrade", "chatgpt", http.StatusSwitchingProtocols, "", http.StatusUpgradeRequired, false},
		{"wrong-endpoint", "chatgpt", http.StatusNotFound, "", http.StatusNotFound, false},
		{"permission-denied", "chatgpt", http.StatusForbidden, "chatpass_v2_scope_not_authorized", http.StatusForbidden, false},
		{"limit", "chatgpt", http.StatusTooManyRequests, "subscription_sharing_usage_limit_exceeded", http.StatusTooManyRequests, false},
		{"chatgpt-route-not-supported", "chatgpt", http.StatusForbidden, "subscription_sharing_route_not_supported", http.StatusUpgradeRequired, false},
		{"codex-route-not-supported", "codex", http.StatusForbidden, "subscription_sharing_route_not_supported", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := transportCatalog(t, tc.channel, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "websocket" ||
					r.Header.Get("Authorization") != "Bearer access-"+tc.channel {
					t.Error("upstream did not receive the bound identity's upgrade request")
				}
				w.WriteHeader(tc.upstream)
				if tc.code != "" {
					_, _ = fmt.Fprintf(w, "{\"error\":{\"code\":%q}}", tc.code)
				}
				if tc.streaming {
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
					}
					<-r.Context().Done()
				}
			})
			adapter := transportAdapter(t, tc.channel, upstream.URL, upstream.Client())
			f := newTransportFixture(t, adapter)
			_, token := f.workspace(tc.name, f.grant(adapter))
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			conn, res, err := f.ws(ctx, token)
			if conn != nil {
				conn.CloseNow()
			}
			if res != nil {
				defer res.Body.Close()
			}
			if err == nil || res == nil || res.StatusCode != tc.downstream || requests.Load() != 1 {
				t.Fatalf("upgrade result: response=%v err=%v upstream requests=%d", res, err, requests.Load())
			}
			if tc.code != "" && tc.downstream != http.StatusUpgradeRequired {
				body, err := io.ReadAll(res.Body)
				if err != nil || !strings.Contains(string(body), tc.code) {
					t.Fatalf("upstream refusal was not preserved: %s (%v)", body, err)
				}
			}
		})
	}
}

func TestGatewaySSEWithoutContentTypeRequiresResponsesFraming(t *testing.T) {
	responses := ": keep-alive\r\n\r\n" +
		"event: response.created\r\nid: initial\r\nretry: 1000\r\ndata: {\"response\":{\"id\":\"resp_fixture\"}}\r\n\r\n" +
		"event: response.completed\r\ndata: " + transportTerminal + "\r\n\r\n"
	message := "event: message\ndata: " + transportTerminal + "\n\n"
	credential := ": access-chatgpt\nid: access-chatgpt\n\ndata: " + transportTerminal + "\n\n"
	nullEvent := "event: response.completed\ndata: null\n\n"
	missingResponse := "data: {\"type\":\"response.completed\"}\n\n"
	missingResponseID := "event: response.completed\ndata: {\"response\":{}}\n\n"
	escapedCredential := "data: " + `{"delta":"\u0061ccess-chatgpt","type":"response.output_text.delta"}` + "\n\ndata: " + transportTerminal + "\n\n"
	escapedDuplicate := "data: " + `{"delta":"\u0061ccess-chatgpt","delta":"safe","type":"response.output_text.delta"}` + "\n\ndata: " + transportTerminal + "\n\n"
	ordinaryEscape := "data: " + `{"delta":"\u0068ello", "type":"response.output_text.delta"}` + "\n\ndata: " + transportTerminal + "\n\n"
	for _, tc := range []struct {
		name, body  string
		contentType []string
		want        int
	}{
		{"responses-stream", responses, nil, http.StatusOK},
		{"cr-only-stream", strings.ReplaceAll(responses, "\r\n", "\r"), nil, http.StatusOK},
		{"bom-before-first-event", "\uFEFF" + message, nil, http.StatusOK},
		{"json-type-with-generic-event-name", message, nil, http.StatusOK},
		{"non-data-credential", credential, nil, http.StatusOK},
		{"escaped-json-credential", escapedCredential, nil, http.StatusOK},
		{"escaped-json-credential-with-duplicate-key", escapedDuplicate, nil, http.StatusOK},
		{"ordinary-json-escape", ordinaryEscape, nil, http.StatusOK},
		{"null-before-valid-terminal", strings.Replace(responses, "event: response.completed", nullEvent+"event: response.completed", 1), nil, http.StatusOK},
		{"missing-response-id-before-valid-terminal", strings.Replace(responses, "event: response.completed", missingResponse+missingResponseID+"event: response.completed", 1), nil, http.StatusOK},
		{"explicit-sse-type", responses, []string{"text/event-stream; charset=utf-8"}, http.StatusOK},
		{"json", `{"type":"response.completed"}`, nil, http.StatusConflict},
		{"html", "<html>not an event stream</html>", nil, http.StatusConflict},
		{"oversized-prefix", strings.Repeat(": keep-alive\n\n", 5000) + "data: " + transportTerminal + "\n\n", nil, http.StatusConflict},
		{"unrelated-event", "event: message\ndata: {\"type\":\"message\"}\n\n", nil, http.StatusConflict},
		{"null-event-data", nullEvent, nil, http.StatusConflict},
		{"terminal-missing-response", missingResponse, nil, http.StatusConflict},
		{"terminal-missing-response-id", missingResponseID, nil, http.StatusConflict},
		{"explicit-non-sse-type", responses, []string{"application/json"}, http.StatusConflict},
		{"explicit-empty-type", responses, []string{""}, http.StatusConflict},
		{"conflicting-types", responses, []string{"", "application/json"}, http.StatusConflict},
		{"invalid-media-parameter", responses, []string{"text/event-stream; charset"}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := transportCatalog(t, "chatgpt", func(w http.ResponseWriter, _ *http.Request) {
				w.Header()["Content-Type"] = tc.contentType
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			adapter := transportAdapter(t, "chatgpt", upstream.URL, upstream.Client())
			f := newTransportFixture(t, adapter)
			_, token := f.workspace(tc.name, f.grant(adapter))
			response, body := f.sse(token, transportInput)
			if response.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.want)
			}
			if tc.want == http.StatusOK {
				wantBody := strings.NewReplacer("\r\n", "\n", "\r", "\n", "\uFEFF", "", "access-chatgpt", "[redacted]", `"delta":"\u0061ccess-chatgpt","delta":"safe"`, `"delta":"safe"`,
					`\u0061ccess-chatgpt`, "[redacted]",
					missingResponse, "", missingResponseID, "",
					`{"response":{"id":"resp_fixture"}}`, `{"response":{"id":"resp_fixture"},"type":"response.created"}`).Replace(tc.body)
				if response.Header.Get("Content-Type") != "text/event-stream" || body != wantBody {
					t.Fatal("SSE changed framing, missed redaction, or did not set the downstream content type")
				}
			} else if !strings.Contains(body, "submission_outcome_unknown") {
				t.Fatal("unrecognized response did not preserve the unknown submission outcome")
			}
		})
	}
}
