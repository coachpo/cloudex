package codexgateway

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestManualCallbackExpiresBeforeExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, service := testGrantGateway(t, "")
		service.redirect = "http://127.0.0.1:1457/auth/callback"
		cfg := codexupstream.OpenAICodexConfig()
		cfg.HTTPClient = &http.Client{Transport: fixtureTrip(func(r *http.Request) (*http.Response, error) {
			t.Errorf("an expired callback tried to contact the configured upstream at %s", r.URL.Path)
			return nil, errors.New("unexpected upstream request")
		})}
		client, err := codexupstream.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		service.clients[codexupstream.ChannelCodex] = client
		ctx := t.Context()
		start, err := service.BeginAuthorization(ctx, "owner", codexupstream.ChannelCodex, "")
		if err != nil {
			t.Fatal(err)
		}
		authorize, err := url.Parse(start.AuthorizationURL)
		if err != nil {
			t.Fatal("invalid fixture authorization URL")
		}
		callback := service.redirect + "?" + url.Values{"code": {"synthetic-code"}, "state": {authorize.Query().Get("state")}}.Encode()
		time.Sleep(authorizationLifetime)
		synctest.Wait()
		if err := service.SubmitAuthorizationCallback(ctx, "owner", start.ID, callback); !errors.Is(err, store.ErrMissing) {
			t.Fatalf("expired attempt accepted a manual callback: %v", err)
		}
		if _, err := service.ConfirmAuthorization(ctx, "owner", start.ID); !errors.Is(err, store.ErrMissing) {
			t.Fatalf("expired attempt could be confirmed: %v", err)
		}
	})
}
