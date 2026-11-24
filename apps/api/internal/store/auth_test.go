package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestResetAdminRequiresSetupAndRevokesSessions(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "auth.sqlite")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const password = "test-password-only"
	expires := time.Now().Add(time.Hour)
	if err := db.CreateAdmin(ctx, "admin", password, "session-before-reset", expires); err != nil {
		t.Fatal(err)
	}
	if err := ResetAdmin(ctx, path); err != nil {
		t.Fatal(err)
	}
	if required, err := db.SetupRequired(ctx); err != nil || !required {
		t.Fatalf("setup required after reset: %v, %v", required, err)
	}
	if _, err := db.Session(ctx, "session-before-reset"); !errors.Is(err, ErrMissing) {
		t.Fatalf("session after reset: got %v, want ErrMissing", err)
	}
	if err := db.CreateAdmin(ctx, "admin", password, "session-after-reset", expires); err != nil {
		t.Fatal(err)
	}
	if user, err := db.Session(ctx, "session-after-reset"); err != nil || user != "admin" {
		t.Fatalf("session after reinitialization: %q, %v", user, err)
	}
}
