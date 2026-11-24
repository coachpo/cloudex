package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/httpapi"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestBrowserOriginCanonicalization(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"http://localhost:3000", "http://localhost:3000"},
		{"http://127.0.0.1:80", "http://127.0.0.1"},
		{"HTTPS://Cloud.Example.invalid:00443/", "https://cloud.example.invalid"},
		{"http://LocalHost:80", "http://localhost"},
		{"https://Cloud.Example.invalid:008443/", "https://cloud.example.invalid:8443"},
		{"https://例.example", "https://xn--fsq.example"},
		{"http://local_console", "http://local_console"},
		{"http://[0:0:0:0:0:0:0:1]:80", "http://[::1]"},
		{"http://[::ffff:127.0.0.1]:80", "http://[::ffff:7f00:1]"},
		{"https://[::1]:65535", "https://[::1]:65535"},
		{"https://cloud.example.invalid:65536", ""},
		{"https://cloud.example.invalid:not-a-port", ""},
		{"https://[fe80::1%25eth0]", ""},
		{"https://[]", ""},
		{"https://:443", ""},
		{"https://admin@cloud.example.invalid", ""},
		{"https://cloud.example.invalid/a", ""},
		{"https://cloud.example.invalid/%2F", ""},
		{"https://cloud.example.invalid?", ""},
		{"https://cloud.example.invalid#", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := browserOrigin(tc.input)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("browserOrigin(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestBrowserOriginRejectsNoncanonicalIPv4(t *testing.T) {
	for _, host := range []string{"127.1", "127.0.0.1.", "0177.0.0.1", "0x7f000001", "2130706433"} {
		origin, err := browserOrigin("http://" + host + ":3000")
		if origin != "" || err == nil || !strings.Contains(err.Error(), "canonical dotted-decimal IPv4 address") {
			t.Errorf("browserOrigin with %q = %q, %v; want guidance on canonical IPv4", host, origin, err)
		}
	}
}

func TestConfiguredOriginAllowsCanonicalLoginWithSecureCookie(t *testing.T) {
	origin, err := browserOrigin("HTTPS://Cloud.Example.invalid:00443/")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const password = "test-password-only"
	if err := db.CreateAdmin(t.Context(), "admin", password, "test-session-digest", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	handler := (&httpapi.Server{Control: &control.Control{Store: db}, Origin: origin}).Handler(http.NotFoundHandler())
	for _, tc := range []struct {
		origin string
		status int
	}{
		{"https://cloud.example.invalid", http.StatusOK},
		{"https://cloud.example.invalid:8443", http.StatusForbidden},
		{"http://cloud.example.invalid", http.StatusForbidden},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://cloud.example.invalid/api/v1/auth/login",
				strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
			req.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("login returned %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			resp := w.Result()
			defer resp.Body.Close()
			cookies := resp.Cookies()
			if tc.status == http.StatusOK && (len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly) {
				t.Fatal("successful HTTPS login did not issue a secure session cookie")
			}
			if tc.status != http.StatusOK && len(cookies) != 0 {
				t.Fatal("rejected login issued a session cookie")
			}
		})
	}
}

func TestResetAdminClearsAuthenticationWithoutChangingBusinessData(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cloud.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const digest = "fixture-session"
	if err := db.CreateAdmin(ctx, "admin", "fixture password", digest, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	op, err := db.Create(ctx, store.Workspace{
		ID: store.ID(), Name: "Existing workspace", Image: "test", Branch: "main",
		Desired: "stopped", CPU: 1000, Memory: 512,
	}, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.Workspace(ctx, op.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLOUDEX_DB_PATH", path)
	t.Setenv("CLOUDEX_MASTER_KEY", "")
	if err := runCommand(ctx, []string{"reset-admin"}); err != nil {
		t.Fatal(err)
	}
	if required, err := db.SetupRequired(ctx); err != nil || !required {
		t.Fatalf("reset did not clear the administrator: setup required=%t, error=%v", required, err)
	}
	if user, err := db.Session(ctx, digest); !errors.Is(err, store.ErrMissing) {
		t.Fatalf("reset did not clear the session: user=%q, error=%v", user, err)
	}
	if after, err := db.Workspace(ctx, before.ID); err != nil || after != before {
		t.Fatalf("reset changed Workspace: before=%+v, after=%+v, error=%v", before, after, err)
	}
	if after, err := db.Operation(ctx, op.ID); err != nil || after != op {
		t.Fatalf("reset changed operation: before=%+v, after=%+v, error=%v", op, after, err)
	}
}

func TestResetAdminRejectsMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	t.Setenv("CLOUDEX_DB_PATH", path)
	t.Setenv("CLOUDEX_MASTER_KEY", "")
	if err := runCommand(t.Context(), []string{"reset-admin"}); err == nil {
		t.Error("reset reported success for a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset should leave the database missing: %v", err)
	}
}
