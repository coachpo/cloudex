package main

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type grantGatewaySettings struct {
	origin, callbackAddress, redirectURI string
}

func readGrantGatewaySettings(console string) (grantGatewaySettings, error) {
	origin := os.Getenv("CLOUDEX_GATEWAY_ORIGIN")
	if origin == "" && strings.HasPrefix(console, "https://") {
		origin = console
	}
	if origin == "" {
		return grantGatewaySettings{}, nil
	}
	origin, err := browserOrigin(origin)
	if err != nil || !strings.HasPrefix(origin, "https://") {
		return grantGatewaySettings{}, errors.New("CLOUDEX_GATEWAY_ORIGIN must be an HTTPS origin")
	}
	uHost := strings.TrimPrefix(console, "http://")
	if strings.HasPrefix(console, "http://") && uHost != "localhost" && uHost != "127.0.0.1" && uHost != "[::1]" &&
		!strings.HasPrefix(uHost, "localhost:") && !strings.HasPrefix(uHost, "127.0.0.1:") && !strings.HasPrefix(uHost, "[::1]:") {
		return grantGatewaySettings{}, errors.New("grant management requires HTTPS or a loopback console")
	}
	port := env("CLOUDEX_OAUTH_CALLBACK_PORT", "1455")
	if port != "1455" && port != "1457" {
		return grantGatewaySettings{}, errors.New("CLOUDEX_OAUTH_CALLBACK_PORT must be 1455 or 1457")
	}
	bind := env("CLOUDEX_OAUTH_CALLBACK_BIND", "127.0.0.1")
	if bind != "127.0.0.1" && bind != "0.0.0.0" {
		return grantGatewaySettings{}, errors.New("CLOUDEX_OAUTH_CALLBACK_BIND must be 127.0.0.1 or 0.0.0.0 for a container published on loopback")
	}
	return grantGatewaySettings{origin: origin, callbackAddress: net.JoinHostPort(bind, port),
		redirectURI: "http://127.0.0.1:" + port + "/auth/callback"}, nil
}

func startGrantGateway(ctx context.Context, db *store.Store, key cipher.AEAD, console string) (*codexgateway.Service, *http.Server, <-chan error, error) {
	settings, err := readGrantGatewaySettings(console)
	if err != nil {
		return nil, nil, nil, err
	}
	var clients []*codexupstream.Client
	for _, cfg := range []codexupstream.Config{codexupstream.OpenAICodexConfig(), codexupstream.ChatGPTConfig()} {
		client, err := codexupstream.New(cfg)
		if err != nil {
			return nil, nil, nil, err
		}
		clients = append(clients, client)
	}
	service, err := codexgateway.New(ctx, codexgateway.Config{
		Store: db, Key: key, Origin: settings.origin, RedirectURI: settings.redirectURI, Clients: clients,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if settings.callbackAddress == "" {
		return service, nil, nil, nil
	}
	listener, err := net.Listen("tcp", settings.callbackAddress)
	if err != nil {
		service.Close()
		return nil, nil, nil, fmt.Errorf("listen for OAuth callback: %w", err)
	}
	expectedHost := strings.TrimSuffix(strings.TrimPrefix(settings.redirectURI, "http://"), "/auth/callback")
	srv := &http.Server{Handler: grantCallback(service, expectedHost),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 20 * time.Second, MaxHeaderBytes: 8 * 1024}
	exited := make(chan error, 1)
	go func() { exited <- srv.Serve(listener) }()
	return service, srv, exited, nil
}

func grantCallback(service *codexgateway.Service, expectedHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != expectedHost || r.URL.Path != "/auth/callback" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		for _, name := range []string{"state", "code", "client_id", "error"} {
			if len(q[name]) > 1 {
				http.Error(w, "Invalid callback.", http.StatusBadRequest)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		if err := service.CompleteAuthorization(ctx, q.Get("state"), q.Get("code"), q.Get("client_id"), q.Get("error")); err != nil {
			http.Error(w, "Sign-in did not complete. Return to the Cloudex console for details.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Identity verified. Return to the Cloudex console to confirm this grant."))
	})
}
