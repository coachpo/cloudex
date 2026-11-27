package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const manualCallbackRedirect = "http://127.0.0.1:1455/auth/callback"

func browserAuthorizationForTest(t *testing.T, console *deviceConsole, fixture *gatewayFixture) (string, url.Values) {
	t.Helper()
	start := deviceData[codexgateway.AuthorizationStart](t, console.call(console.owner, http.MethodPost, deviceAttempts,
		map[string]string{"channel": fixture.channel}, http.StatusCreated))
	authorize, err := url.Parse(start.AuthorizationURL)
	if err != nil || authorize.Query().Get("redirect_uri") != manualCallbackRedirect {
		t.Fatal("browser authorization omitted the original redirect URI")
	}
	state, code, issued := fixture.issueCode(start.AuthorizationURL)
	query := url.Values{"state": {state}, "code": {code}}
	if issued != "" {
		query.Set("client_id", issued)
	}
	return deviceAttempts + "/" + start.ID, query
}

func callbackPayload(address string, query url.Values) map[string]string {
	return map[string]string{"callback_url": address + "?" + query.Encode()}
}

func assertCallbackProblem(t *testing.T, response *httptest.ResponseRecorder, code string, secrets ...string) {
	t.Helper()
	var envelope struct{ Error struct{ Code string } }
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != code {
		t.Fatalf("expected callback error %q, got %q: %v", code, envelope.Error.Code, err)
	}
	for _, secret := range secrets {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("callback error disclosed submitted OAuth material")
		}
	}
}

func TestGrantManualCallbackBothChannels(t *testing.T) {
	for _, channel := range []string{codexupstream.ChannelCodex, codexupstream.ChannelChatGPT} {
		t.Run(channel, func(t *testing.T) {
			fixture := newGatewayFixture(t, channel, fixtureFault{})
			// Only the synthetic upstream has an HTTP listener. The console is
			// exercised in process, with no callback listener or SSH tunnel.
			console, _ := newGrantTestConsole(t, manualCallbackRedirect, fixture.adapter())
			path, query := browserAuthorizationForTest(t, console, fixture)
			query.Set("redirect_uri", "https://unused.example.test/callback")
			query.Set("iss", "https://unused.example.test")
			payload := callbackPayload(manualCallbackRedirect, query)
			w := console.call(console.owner, http.MethodPost, path+"/callback", payload, http.StatusNoContent)
			if w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 || fixture.exchanges.Load() != 1 {
				t.Fatal("manual callback must exchange once without returning OAuth material")
			}
			ready := console.wait(path, "ready")
			if ready.Identity == nil || ready.Identity.Channel != channel || ready.Identity.Subject != "fixture-"+channel+"-subject" || ready.Identity.Credential != nil {
				t.Fatal("manual callback did not prepare the expected identity")
			}
			if channel == codexupstream.ChannelCodex && ready.Identity.UpstreamWorkspaceID != fixture.account ||
				channel == codexupstream.ChannelChatGPT && ready.Identity.ClientID != query.Get("client_id") {
				t.Fatal("manual callback returned the wrong workspace or registration")
			}
			console.count(0)
			w = console.call(console.owner, http.MethodPost, path+"/callback", payload, http.StatusBadRequest)
			assertCallbackProblem(t, w, "invalid_oauth_state", query.Get("code"), query.Get("state"))
			console.call(console.other, http.MethodGet, path, nil, http.StatusNotFound)
			console.call(console.other, http.MethodPost, path+"/confirm", nil, http.StatusNotFound)
			grant := deviceData[store.Grant](t, console.call(console.owner, http.MethodPost, path+"/confirm", nil, http.StatusOK))
			if grant.Subject != ready.Identity.Subject || grant.Channel != channel || fixture.exchanges.Load() != 1 {
				t.Fatal("confirmation changed the verified identity or repeated the exchange")
			}
			console.call(console.owner, http.MethodPost, path+"/confirm", nil, http.StatusNotFound)
			console.count(1)
		})
	}
}

func TestGrantManualCallbackRejectsInvalidInputBeforeExchange(t *testing.T) {
	fixture := newGatewayFixture(t, codexupstream.ChannelChatGPT, fixtureFault{})
	console, _ := newGrantTestConsole(t, manualCallbackRedirect, fixture.adapter())
	path, query := browserAuthorizationForTest(t, console, fixture)
	payload := callbackPayload(manualCallbackRedirect, query)
	console.call(nil, http.MethodPost, path+"/callback", payload, http.StatusUnauthorized)
	w := console.call(console.other, http.MethodPost, path+"/callback", payload, http.StatusNotFound)
	assertCallbackProblem(t, w, "NOT_FOUND", query.Get("code"), query.Get("state"))
	var fetched atomic.Int32
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(decoy.Close)
	for _, address := range []string{
		"https://127.0.0.1:1455/auth/callback",
		"http://localhost:1455/auth/callback",
		"http://127.0.0.1:1457/auth/callback",
		"http://127.0.0.1:1455/auth/%63allback",
		"http://127.0.0.1:1455/callback",
		decoy.URL + "/auth/callback",
	} {
		w := console.call(console.owner, http.MethodPost, path+"/callback", callbackPayload(address, query), http.StatusBadRequest)
		assertCallbackProblem(t, w, "oauth_callback_address_mismatch", query.Get("code"), query.Get("state"))
	}
	for _, input := range []string{
		"/auth/callback?" + query.Encode(),
		"http://user@127.0.0.1:1455/auth/callback?" + query.Encode(),
		payload["callback_url"] + "#fragment",
		payload["callback_url"] + "&state=second-state",
		payload["callback_url"] + "&unsafe=%ZZ",
		manualCallbackRedirect + "?state=" + query.Get("state"),
	} {
		w := console.call(console.owner, http.MethodPost, path+"/callback", map[string]string{"callback_url": input}, http.StatusBadRequest)
		assertCallbackProblem(t, w, "invalid_oauth_callback_url", query.Get("code"), query.Get("state"))
	}
	_, otherQuery := browserAuthorizationForTest(t, console, fixture)
	w = console.call(console.owner, http.MethodPost, path+"/callback", callbackPayload(manualCallbackRedirect, otherQuery), http.StatusBadRequest)
	assertCallbackProblem(t, w, "invalid_oauth_state", otherQuery.Get("code"), otherQuery.Get("state"))
	if fixture.exchanges.Load() != 0 || fetched.Load() != 0 {
		t.Fatal("rejected callback caused outbound traffic")
	}
	console.call(console.owner, http.MethodPost, path+"/callback", payload, http.StatusNoContent)
	if fixture.exchanges.Load() != 1 {
		t.Fatal("rejected submissions consumed the valid callback")
	}
}

func TestGrantManualCallbackProviderErrors(t *testing.T) {
	fixture := newGatewayFixture(t, codexupstream.ChannelChatGPT, fixtureFault{})
	console, _ := newGrantTestConsole(t, manualCallbackRedirect, fixture.adapter())
	path, query := browserAuthorizationForTest(t, console, fixture)
	query.Del("code")
	query.Set("error", "access_denied")
	query.Set("error_description", "synthetic private provider details")
	w := console.call(console.owner, http.MethodPost, path+"/callback", callbackPayload(manualCallbackRedirect, query), http.StatusBadRequest)
	assertCallbackProblem(t, w, "authorization_denied", query.Get("state"), query.Get("error_description"))
	if failed := console.wait(path, "failed"); failed.Code != "authorization_denied" {
		t.Fatal("provider error did not stop the authorization")
	}
	path, query = browserAuthorizationForTest(t, console, fixture)
	query.Del("client_id")
	w = console.call(console.owner, http.MethodPost, path+"/callback", callbackPayload(manualCallbackRedirect, query), http.StatusConflict)
	assertCallbackProblem(t, w, "oauth_identity_mismatch", query.Get("code"), query.Get("state"))
	console.call(console.owner, http.MethodPost, path+"/confirm", nil, http.StatusConflict)
	if fixture.exchanges.Load() != 0 {
		t.Fatal("provider denial or incomplete ChatGPT registration reached the token endpoint")
	}
	console.count(0)
}

func TestGrantManualCallbackRacesAutomaticCallback(t *testing.T) {
	for _, first := range []string{"manual", "automatic"} {
		t.Run(first+" first", func(t *testing.T) {
			started, release := make(chan struct{}, 2), make(chan struct{})
			fixture := newGatewayFixture(t, codexupstream.ChannelCodex, fixtureFault{beforeExchange: func(ctx context.Context) {
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}})
			console, service := newGrantTestConsole(t, manualCallbackRedirect, fixture.adapter())
			path, query := browserAuthorizationForTest(t, console, fixture)
			automatic := grantCallback(service, "127.0.0.1:1455")
			request := func(method string) int {
				if method == "manual" {
					return console.request(console.owner, http.MethodPost, path+"/callback", callbackPayload(manualCallbackRedirect, query)).Code
				}
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, manualCallbackRedirect+"?"+query.Encode(), nil)
				w := httptest.NewRecorder()
				automatic.ServeHTTP(w, r)
				return w.Code
			}
			firstResult := make(chan int, 1)
			go func() { firstResult <- request(first) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("first callback did not reach the fake upstream")
			}
			second, success := "manual", http.StatusOK
			if first == "manual" {
				second, success = "automatic", http.StatusNoContent
			}
			secondResult := make(chan int, 1)
			go func() { secondResult <- request(second) }()
			select {
			case status := <-secondResult:
				close(release)
				if status != http.StatusBadRequest {
					t.Fatalf("concurrent callback returned %d, want 400", status)
				}
			case <-started:
				close(release)
				t.Fatal("concurrent callback reached the token endpoint")
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("concurrent callback did not return while exchange was in flight")
			}
			if status := <-firstResult; status != success || fixture.exchanges.Load() != 1 {
				t.Fatalf("first callback returned %d with %d exchanges", status, fixture.exchanges.Load())
			}
			console.count(0)
			console.call(console.owner, http.MethodPost, path+"/confirm", nil, http.StatusOK)
			console.call(console.owner, http.MethodPost, path+"/confirm", nil, http.StatusNotFound)
			console.count(1)
		})
	}
}
