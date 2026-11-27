package codexgateway

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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestDeviceAuthorizationExpiresDuringPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, service := testGrantGateway(t, "")
		defer service.Close()
		var polls int
		var pollErr error
		cfg := codexupstream.OpenAICodexConfig()
		cfg.HTTPClient = &http.Client{Transport: fixtureTrip(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			switch r.URL.Path {
			case "/api/accounts/deviceauth/usercode":
				w.WriteString(`{"user_code":"TEST-CODE","device_auth_id":"synthetic-id","interval":"890"}`)
			case "/api/accounts/deviceauth/token":
				polls++
				if polls == 1 {
					w.WriteHeader(http.StatusNotFound)
				} else {
					<-r.Context().Done()
					pollErr = r.Context().Err()
					w.WriteString(`{"authorization_code":"late-code","code_challenge":"synthetic-challenge","code_verifier":"synthetic-verifier"}`)
				}
			default:
				t.Errorf("an expired poll must not reach %s", r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
			}
			return w.Result(), nil
		})}
		client, err := codexupstream.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		service.clients[codexupstream.ChannelCodex] = client
		ctx := t.Context()
		start, err := service.BeginDeviceAuthorization(ctx, "owner", codexupstream.ChannelCodex, "")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(890 * time.Second)
		synctest.Wait()
		if status, err := service.AuthorizationStatus("owner", start.ID); err != nil || status.Status != "pending" || polls != 2 {
			t.Fatalf("before expiry: status %q with %d polls: %v", status.Status, polls, err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if !errors.Is(pollErr, context.DeadlineExceeded) || polls != 2 {
			t.Fatalf("15-minute expiry did not stop polling, got %v and %d polls", pollErr, polls)
		}
		if _, err := service.AuthorizationStatus("owner", start.ID); !errors.Is(err, store.ErrMissing) {
			t.Fatalf("expired attempt remained visible: %v", err)
		}
		if _, err := service.ConfirmAuthorization(ctx, "owner", start.ID); !errors.Is(err, store.ErrMissing) {
			t.Fatalf("expired attempt could be confirmed: %v", err)
		}
		if grants, err := service.Store.Grants(ctx); err != nil || len(grants) != 0 {
			t.Fatalf("expired attempt persisted a grant: %v", err)
		}
	})
}

func TestDeviceAuthorizationLateTokenExchange(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		exchangeErr error
	}{
		{"active", nil},
		{"cancelled", context.Canceled},
		{"expired", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, service := testGrantGateway(t, "")
				cfg := codexupstream.OpenAICodexConfig()
				tokens, jwks := deviceExchangeFixture(t, key, cfg.Issuer)
				exchangeRelease := make(chan struct{})
				release := sync.OnceFunc(func() { close(exchangeRelease) })
				defer release()
				var polls, exchanges, responses int
				var exchangeErr error
				cfg.HTTPClient = &http.Client{Transport: fixtureTrip(func(r *http.Request) (*http.Response, error) {
					w := httptest.NewRecorder()
					switch r.URL.Path {
					case "/api/accounts/deviceauth/usercode":
						w.WriteString(`{"user_code":"TEST-CODE","device_auth_id":"synthetic-id","interval":"890"}`)
					case "/api/accounts/deviceauth/token":
						polls++
						if polls == 1 {
							w.WriteHeader(http.StatusNotFound)
						} else {
							w.WriteString(`{"authorization_code":"synthetic-code","code_challenge":"synthetic-challenge","code_verifier":"synthetic-verifier"}`)
						}
					case "/oauth/token":
						exchanges++
						<-exchangeRelease
						exchangeErr = r.Context().Err()
						// Model a successful response that arrives despite cancellation.
						w.Write(tokens)
						responses++
					case "/.well-known/jwks.json":
						w.Write(jwks)
					default:
						t.Errorf("unexpected device authorization request to %s", r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
					}
					return w.Result(), nil
				})}
				client, err := codexupstream.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				service.clients[codexupstream.ChannelCodex] = client
				ctx := t.Context()
				start, err := service.BeginDeviceAuthorization(ctx, "owner", codexupstream.ChannelCodex, "")
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(890 * time.Second)
				synctest.Wait()
				status, err := service.AuthorizationStatus("owner", start.ID)
				if err != nil || status.Status != "pending" || polls != 2 || exchanges != 1 || responses != 0 {
					t.Fatalf("before invalidation: status %q, polls %d, exchanges %d, responses %d: %v",
						status.Status, polls, exchanges, responses, err)
				}
				switch tc.exchangeErr {
				case context.Canceled:
					if err := service.CancelDeviceAuthorization("owner", start.ID); err != nil {
						t.Fatal(err)
					}
				case context.DeadlineExceeded:
					time.Sleep(10 * time.Second)
					synctest.Wait()
				}
				release()
				synctest.Wait()
				if !errors.Is(exchangeErr, tc.exchangeErr) || polls != 2 || exchanges != 1 || responses != 1 {
					t.Fatalf("after response: exchange context %v, polls %d, exchanges %d, responses %d",
						exchangeErr, polls, exchanges, responses)
				}
				if tc.exchangeErr == nil {
					grant, err := service.ConfirmAuthorization(ctx, "owner", start.ID)
					if err != nil || grant.Subject != "fixture-device-owner" {
						t.Fatalf("active attempt could not save the fixture identity: %v", err)
					}
					return
				}
				if _, err := service.AuthorizationStatus("owner", start.ID); !errors.Is(err, store.ErrMissing) {
					t.Fatalf("invalidated attempt remained visible: %v", err)
				}
				if _, err := service.ConfirmAuthorization(ctx, "owner", start.ID); !errors.Is(err, store.ErrMissing) {
					t.Fatalf("invalidated attempt could be confirmed: %v", err)
				}
				if grants, err := service.Store.Grants(ctx); err != nil || len(grants) != 0 {
					t.Fatalf("invalidated attempt persisted a grant: %v", err)
				}
			})
		})
	}
}

func deviceExchangeFixture(t *testing.T, key *rsa.PrivateKey, issuer string) (tokens, jwks []byte) {
	t.Helper()
	encoding := base64.RawURLEncoding
	claims, err := json.Marshal(map[string]any{
		"iss": issuer, "aud": codexupstream.CodexClientID, "sub": "fixture-device-owner", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]string{"chatgpt_user_id": "fixture-user", "chatgpt_account_id": "fixture-workspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := encoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture-key"}`)) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	jwt := payload + "." + encoding.EncodeToString(signature)
	tokens, err = json.Marshal(map[string]string{"id_token": jwt, "access_token": jwt, "refresh_token": "synthetic-refresh-token"})
	if err != nil {
		t.Fatal(err)
	}
	jwks, err = json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "fixture-key", "kty": "RSA", "n": encoding.EncodeToString(key.N.Bytes()),
		"e": encoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return tokens, jwks
}
