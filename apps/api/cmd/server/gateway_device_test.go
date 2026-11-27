package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/httpapi"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const (
	deviceAttempts = "/grant-authorizations"
	deviceCode     = "TEST-CODE"
	deviceID       = "synthetic-device-auth-id"
)

func issueFixtureDeviceCode(f *gatewayFixture, w http.ResponseWriter, subject, account string, n int32) {
	code := fmt.Sprintf("synthetic-device-authorization-code-%d", n)
	verifier := "synthetic-device-code-verifier"
	challenge := sha256.Sum256([]byte(verifier))
	encoded := base64.RawURLEncoding.EncodeToString(challenge[:])
	f.mu.Lock()
	f.codes[code] = fixtureOAuth{
		challenge: encoded, subject: subject, account: account, redirect: "https://auth.openai.com/deviceauth/callback",
	}
	f.mu.Unlock()
	writeFixtureJSON(w, map[string]string{"authorization_code": code, "code_challenge": encoded, "code_verifier": verifier})
}

func deviceFixtureHandler(t *testing.T, startStatus int, poll func(*gatewayFixture, http.ResponseWriter, *http.Request)) *gatewayFixture {
	t.Helper()
	return newGatewayFixtureWithDevice(t, codexupstream.ChannelCodex, fixtureFault{}, func(f *gatewayFixture, w http.ResponseWriter, r *http.Request) {
		var fields map[string]string
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&fields) != nil {
			t.Error("expected a JSON POST to the device endpoint")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/api/accounts/deviceauth/usercode" {
			if len(fields) != 1 || fields["client_id"] != codexupstream.CodexClientID {
				t.Error("device start did not use the fixed Codex client ID")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if startStatus != http.StatusOK {
				w.WriteHeader(startStatus)
				return
			}
			writeFixtureJSON(w, map[string]string{"usercode": deviceCode, "device_auth_id": deviceID, "interval": "1"})
			return
		}
		if len(fields) != 2 || fields["device_auth_id"] != deviceID || fields["user_code"] != deviceCode {
			t.Error("device poll did not use the issued device ID and user code")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if poll == nil {
			t.Error("unexpected device poll")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		poll(f, w, r)
	})
}

type deviceConsole struct {
	t     *testing.T
	api   http.Handler
	store *store.Store
	owner *http.Cookie
	other *http.Cookie
}

func newDeviceConsole(t *testing.T, fixture *gatewayFixture) *deviceConsole {
	t.Helper()
	console, _ := newGrantTestConsole(t, "", fixture.adapter())
	return console
}

func newGrantTestConsole(t *testing.T, redirectURI string, clients ...*codexupstream.Client) (*deviceConsole, *codexgateway.Service) {
	t.Helper()
	db, key := acceptanceStore(t, t.Context(), t.TempDir())
	service, err := codexgateway.New(t.Context(), codexgateway.Config{
		Store: db, Key: key, Origin: "https://console.example.test", RedirectURI: redirectURI, Clients: clients,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	api := (&httpapi.Server{Control: &control.Control{Store: db, Key: key}, Origin: "https://console.example.test", GrantGateway: service}).Handler(nil)
	c := &deviceConsole{t: t, api: api, store: db}
	login := map[string]string{"username": "admin", "password": "synthetic-password"}
	c.owner = c.call(nil, http.MethodPost, "/auth/setup", login, http.StatusOK).Result().Cookies()[0]
	c.other = c.call(nil, http.MethodPost, "/auth/login", login, http.StatusOK).Result().Cookies()[0]
	return c, service
}

func (c *deviceConsole) call(session *http.Cookie, method, path string, in any, want int) *httptest.ResponseRecorder {
	c.t.Helper()
	w := c.request(session, method, path, in)
	if w.Code != want {
		c.t.Fatalf("%s %s got %d, want %d: %s", method, path, w.Code, want, w.Body)
	}
	return w
}

func (c *deviceConsole) request(session *http.Cookie, method, path string, in any) *httptest.ResponseRecorder {
	c.t.Helper()
	var body []byte
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			c.t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(c.t.Context())
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, method, "https://console.example.test/api/v1"+path, bytes.NewReader(body))
	r.Header.Set("Origin", "https://console.example.test")
	r.Header.Set("Content-Type", "application/json")
	if session != nil {
		r.AddCookie(session)
	}
	w := httptest.NewRecorder()
	c.api.ServeHTTP(w, r)
	return w
}

func deviceData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var envelope struct{ Data T }
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func (c *deviceConsole) begin(grantID string) string {
	c.t.Helper()
	input := map[string]string{"channel": "codex", "method": "device_code", "grant_id": grantID}
	w := c.call(c.owner, http.MethodPost, deviceAttempts, input, http.StatusCreated)
	result := deviceData[map[string]json.RawMessage](c.t, w)
	var id string
	var prompt map[string]string
	if len(result) != 2 || json.Unmarshal(result["id"], &id) != nil || id == "" ||
		json.Unmarshal(result["device"], &prompt) != nil || len(prompt) != 2 ||
		prompt["verification_url"] != "https://auth.openai.com/codex/device" || prompt["user_code"] != deviceCode {
		c.t.Fatal("device start must expose only the attempt ID, verification URL and user code")
	}
	return deviceAttempts + "/" + id
}

func (c *deviceConsole) wait(path, want string) codexgateway.AuthorizationStatus {
	c.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		result := deviceData[codexgateway.AuthorizationStatus](c.t, c.call(c.owner, http.MethodGet, path, nil, http.StatusOK))
		if result.Status == want {
			return result
		}
		if result.Status != "pending" || time.Now().After(deadline) {
			c.t.Fatalf("authorization is %q, want %q (code %q)", result.Status, want, result.Code)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *deviceConsole) count(want int) {
	c.t.Helper()
	grants, err := c.store.Grants(c.t.Context())
	if err != nil || len(grants) != want {
		c.t.Fatalf("persisted %d grants, want %d: %v", len(grants), want, err)
	}
}

func TestGrantDeviceCode(t *testing.T) {
	t.Run("confirm and reauthorize without a callback", func(t *testing.T) {
		var polls atomic.Int32
		var previousPoll atomic.Pointer[time.Time]
		allowCode := make(chan struct{})
		fixture := deviceFixtureHandler(t, http.StatusOK, func(f *gatewayFixture, w http.ResponseWriter, r *http.Request) {
			n := polls.Add(1)
			now := time.Now()
			previous := previousPoll.Swap(&now)
			if (n == 2 || n == 3) && now.Sub(*previous) < time.Second {
				t.Error("poll ignored the upstream interval")
			}
			switch n {
			case 1:
				w.WriteHeader(http.StatusForbidden)
			case 2:
				w.WriteHeader(http.StatusNotFound)
			case 3:
				select {
				case <-allowCode:
					issueFixtureDeviceCode(f, w, "device-subject", f.account, n)
				case <-r.Context().Done():
				}
			case 4:
				issueFixtureDeviceCode(f, w, "other-device-subject", f.account, n)
			case 5:
				issueFixtureDeviceCode(f, w, "device-subject", "other-workspace", n)
			default:
				issueFixtureDeviceCode(f, w, "device-subject", f.account, n)
			}
		})
		c := newDeviceConsole(t, fixture)
		for _, in := range []map[string]string{{"channel": "chatgpt", "method": "device_code"}, {"channel": "codex", "method": "unknown"}} {
			c.call(c.owner, http.MethodPost, deviceAttempts, in, http.StatusBadRequest)
		}
		if fixture.requests.Load() != 0 {
			t.Fatal("invalid sign-in choices reached the upstream")
		}
		path := c.begin("")
		c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusConflict)
		close(allowCode)
		ready := c.wait(path, "ready")
		if ready.Identity == nil || ready.Identity.Subject != "device-subject" || ready.Identity.UpstreamWorkspaceID != fixture.account || ready.Identity.Credential != nil {
			t.Fatal("device status omitted the verified identity or exposed credentials")
		}
		c.call(c.other, http.MethodGet, path, nil, http.StatusNotFound)
		c.call(c.other, http.MethodPost, path+"/confirm", nil, http.StatusNotFound)
		c.call(c.other, http.MethodDelete, path, nil, http.StatusNotFound)
		c.count(0)
		grant := deviceData[store.Grant](t, c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusOK))
		c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusNotFound)
		c.count(1)

		for _, mismatch := range []string{"account", "workspace"} {
			path = c.begin(grant.ID)
			if failed := c.wait(path, "failed"); failed.Code != "oauth_identity_mismatch" {
				t.Fatalf("unexpected %s mismatch error %q", mismatch, failed.Code)
			}
			c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusConflict)
			unchanged, err := c.store.Grant(t.Context(), grant.ID)
			if err != nil || unchanged.GrantEpoch != grant.GrantEpoch {
				t.Fatalf("wrong %s changed the original grant: %v", mismatch, err)
			}
		}
		path = c.begin(grant.ID)
		c.wait(path, "ready")
		replaced := deviceData[store.Grant](t, c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusOK))
		if replaced.ID != grant.ID || replaced.GrantEpoch != grant.GrantEpoch+1 {
			t.Fatal("reauthorization did not replace the original grant")
		}
		c.count(1)
	})
	t.Run("cancel an in-flight poll", func(t *testing.T) {
		started, cancelled := make(chan struct{}), make(chan struct{})
		var polls atomic.Int32
		fixture := deviceFixtureHandler(t, http.StatusOK, func(f *gatewayFixture, w http.ResponseWriter, r *http.Request) {
			if n := polls.Add(1); n != 1 {
				t.Error("unexpected poll after cancellation")
				return
			}
			close(started)
			<-r.Context().Done()
			issueFixtureDeviceCode(f, w, "late-device-subject", f.account, 1)
			close(cancelled)
		})
		c := newDeviceConsole(t, fixture)
		path := c.begin("")
		waitDeviceSignal(t, started)
		c.call(c.owner, http.MethodDelete, path, nil, http.StatusNoContent)
		waitDeviceSignal(t, cancelled)
		c.call(c.owner, http.MethodGet, path, nil, http.StatusNotFound)
		c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusNotFound)
		if fixture.requests.Load() != 2 {
			t.Fatal("cancelled attempt reached the upstream after polling")
		}
		c.count(0)
	})
	t.Run("upstream disables or refuses device login", func(t *testing.T) {
		fixture := deviceFixtureHandler(t, http.StatusNotFound, nil)
		c := newDeviceConsole(t, fixture)
		w := c.call(c.owner, http.MethodPost, deviceAttempts, map[string]string{"channel": "codex", "method": "device_code"}, http.StatusForbidden)
		if !bytes.Contains(w.Body.Bytes(), []byte(`"code":"device_code_unavailable"`)) || fixture.requests.Load() != 1 {
			t.Fatal("device start rejection did not end the attempt")
		}
		c.count(0)
	})
	t.Run("upstream denies a pending attempt", func(t *testing.T) {
		fixture := deviceFixtureHandler(t, http.StatusOK, func(_ *gatewayFixture, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			writeFixtureJSON(w, map[string]string{"error": "access_denied", "error_description": "private upstream details"})
		})
		c := newDeviceConsole(t, fixture)
		path := c.begin("")
		failed := c.wait(path, "failed")
		if failed.Code != "device_code_authorization_failed" || failed.Error == "private upstream details" || fixture.requests.Load() != 2 {
			t.Fatal("device denial did not stop before exchange with a sanitized error")
		}
		c.call(c.owner, http.MethodPost, path+"/confirm", nil, http.StatusConflict)
		c.count(0)
	})
}

func waitDeviceSignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the device upstream")
	}
}
