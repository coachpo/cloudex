package store

import (
	"errors"
	"testing"
)

func TestCodexConfigSaveAndAppliedRevisions(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	w, _ := makeRunning(t, s)
	settings := CodexConfigSettings{
		Managed: true, AgentsMD: "# Project\n", ConfigTOML: "model = 'fixture'\n",
		AuthJSON: "{\n  \"fixture\": \"fake-login\"\n}\n",
	}
	want := CodexConfig{CodexConfigSettings: settings, Revision: 1}
	if saved, err := s.SaveCodexConfig(ctx, w.ID, 0, settings); err != nil || saved != want {
		t.Fatalf("first save = %+v, %v", saved, err)
	}
	var storedAuth string
	if err := s.db.QueryRowContext(ctx, "SELECT auth_json FROM workspace_codex_config WHERE workspace_id=?", w.ID).Scan(&storedAuth); err != nil || storedAuth != settings.AuthJSON {
		t.Fatalf("auth.json did not persist as plain text: %v", err)
	}
	if saved, err := s.SaveCodexConfig(ctx, w.ID, want.Revision, settings); err != nil || saved != want {
		t.Fatalf("unchanged save = %+v, %v", saved, err)
	}
	if err := s.MarkCodexConfigApplied(ctx, w.ID, want.Revision); err != nil {
		t.Fatal(err)
	}
	settings.ConfigTOML = "# updated\n"
	if _, err := s.SaveCodexConfig(ctx, w.ID, 0, settings); !errors.Is(err, ErrCodexConfigChanged) {
		t.Fatalf("stale save: %v", err)
	}
	want = CodexConfig{CodexConfigSettings: settings, Revision: 2, AppliedRevision: 1}
	if saved, err := s.SaveCodexConfig(ctx, w.ID, 1, settings); err != nil || saved != want {
		t.Fatalf("updated save = %+v, %v", saved, err)
	}
	if err := s.MarkCodexConfigApplied(ctx, w.ID, 1); !errors.Is(err, ErrCodexConfigChanged) {
		t.Fatalf("stale applied revision: %v", err)
	}
	if got, err := s.CodexConfig(ctx, w.ID); err != nil || got != want {
		t.Fatalf("reloaded config = %+v, %v", got, err)
	}
}

func TestCodexConfigRejectsQueuedAndRunningOperations(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	w, _ := makeRunning(t, s)
	if _, err := s.Enqueue(ctx, w.ID, "stop", false, "", ""); err != nil {
		t.Fatal(err)
	}
	settings := CodexConfigSettings{Managed: true}
	if _, err := s.SaveCodexConfig(ctx, w.ID, 0, settings); !errors.Is(err, ErrBusy) {
		t.Fatalf("save during queued operation: %v", err)
	}
	if _, _, err := s.ClaimOp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveCodexConfig(ctx, w.ID, 0, settings); !errors.Is(err, ErrBusy) {
		t.Fatalf("save during running operation: %v", err)
	}
}
