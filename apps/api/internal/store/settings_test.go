package store

import (
	"path/filepath"
	"testing"
)

func TestSettingsPersistAcrossReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "settings.sqlite")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if settings, err := db.Settings(ctx); err != nil || settings != (Settings{WorkspaceImage: DefaultWorkspaceImage}) {
		t.Fatalf("default settings = %+v, %v", settings, err)
	}
	want := Settings{
		SSHHost: "gateway.example.org", AgentPublicAddress: "[2001:db8::1]:9443",
		WorkspaceImage: "workspace:next",
	}
	if err := db.SetAccessSettings(ctx, want.SSHHost, want.AgentPublicAddress); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkspaceImage(ctx, want.WorkspaceImage); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if settings, err := reopened.Settings(ctx); err != nil || settings != want {
		t.Fatalf("persisted settings = %+v, %v; want %+v", settings, err, want)
	}
}
