package main

import (
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
)

func TestLocalGatewayConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, console, explicit, enabled, origin string
		local                                    bool
	}{
		{"Compose loopback", "http://localhost:3000", "", "1", runtime.LocalGatewayOrigin, true},
		{"explicit override", "http://localhost:3000", "https://gateway.example.test", "1", "https://gateway.example.test", false},
		{"HTTPS console", "https://console.example.test", "", "1", "https://console.example.test", false},
		{"public HTTP console", "http://console.example.test", "", "1", "", false},
		{"standalone HTTP console", "http://localhost:3000", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUDEX_GATEWAY_ORIGIN", tc.explicit)
			t.Setenv("CLOUDEX_LOCAL_GATEWAY", tc.enabled)
			t.Setenv("CLOUDEX_OAUTH_CALLBACK_PORT", "")
			t.Setenv("CLOUDEX_OAUTH_CALLBACK_BIND", "cloudex-control")
			got, err := readGrantGatewaySettings(tc.console)
			if err != nil || got.local != tc.local || got.origin != tc.origin {
				t.Fatalf("gateway settings = %+v, %v", got, err)
			}
			if tc.local && (got.callbackAddress != "cloudex-control:1455" ||
				got.redirectURI != "http://127.0.0.1:1455/auth/callback") {
				t.Fatalf("unexpected local OAuth callback settings: %+v", got)
			}
		})
	}
}

func TestConfiguredGatewayStillRequiresHTTPS(t *testing.T) {
	t.Setenv("CLOUDEX_GATEWAY_ORIGIN", "http://gateway.example.test")
	t.Setenv("CLOUDEX_LOCAL_GATEWAY", "1")
	if _, err := readGrantGatewaySettings("http://localhost:3000"); err == nil {
		t.Fatal("accepted a configured gateway without HTTPS")
	}
}
