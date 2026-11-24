package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const codexFilesPath = "/settings/codex-files"

func TestInitialCodexFilesAPI(t *testing.T) {
	api := newTestAPI(t, &control.Control{Store: newDB(t)})
	want := store.InitialCodexFiles{AgentsMD: "# Project guidance\n", ConfigTOML: "# retained comment\nmodel = 'fixture'\n"}
	api.call(http.MethodGet, codexFilesPath, nil, http.StatusUnauthorized, nil)
	api.call(http.MethodPut, codexFilesPath, want, http.StatusUnauthorized, nil)
	api.login()

	var files store.InitialCodexFiles
	api.call(http.MethodGet, codexFilesPath, nil, http.StatusOK, &files)
	if files != (store.InitialCodexFiles{}) {
		t.Fatalf("unexpected defaults: %+v", files)
	}
	api.expectProblem(http.MethodPut, codexFilesPath, map[string]string{
		"agents_md": "", "config_toml": "", "auth_json": "{}",
	}, http.StatusBadRequest, "INVALID_REQUEST")
	api.call(http.MethodPut, codexFilesPath, want, http.StatusOK, &files)
	if files != want {
		t.Fatalf("saved files: %+v", files)
	}
	api.call(http.MethodGet, codexFilesPath, nil, http.StatusOK, &files)
	if files != want {
		t.Fatalf("read files: %+v", files)
	}
	api.call(http.MethodPut, codexFilesPath, store.InitialCodexFiles{}, http.StatusOK, nil)
	api.call(http.MethodGet, codexFilesPath, nil, http.StatusOK, &files)
	if files != (store.InitialCodexFiles{}) {
		t.Fatalf("files were not cleared: %+v", files)
	}
}

func TestInitialCodexFilesRejectsInvalidInput(t *testing.T) {
	api := newTestAPI(t, &control.Control{Store: newDB(t)})
	api.login()
	for _, tc := range []struct {
		name, field string
		value       any
		code        string
	}{
		{"instructions missing", "agents_md", nil, "INVALID_CODEX_CONFIG"},
		{"instructions must be a string", "agents_md", 42, "INVALID_REQUEST"},
		{"TOML must be a string", "config_toml", map[string]any{}, "INVALID_REQUEST"},
		{"instructions too large", "agents_md", strings.Repeat("a", maxInitialCodexFileBytes+1), "INVALID_CODEX_CONFIG"},
		{"instructions with NUL", "agents_md", "# invalid\x00", "INVALID_CODEX_CONFIG"},
		{"invalid TOML", "config_toml", "model =", "INVALID_CODEX_CONFIG"},
		{"unsupported credential store", "config_toml", `cli_auth_credentials_store = "keyring"`, "INVALID_CODEX_CONFIG"},
		{"credential store must be a string", "config_toml", "cli_auth_credentials_store = true", "INVALID_CODEX_CONFIG"},
		{"TOML too large", "config_toml", "#" + strings.Repeat("a", maxInitialCodexFileBytes), "INVALID_CODEX_CONFIG"},
		{"TOML with NUL", "config_toml", "# invalid\x00", "INVALID_CODEX_CONFIG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := api
			api.t = t
			request := map[string]any{"agents_md": "", "config_toml": runtime.CodexConfigSeed}
			request[tc.field] = tc.value
			api.expectProblem(http.MethodPut, codexFilesPath, request, http.StatusBadRequest, tc.code)
		})
	}
}

func TestInitialCodexFilesAcceptsEscapedInputAtLimits(t *testing.T) {
	api := newTestAPI(t, &control.Control{Store: newDB(t)})
	api.login()
	// Go's JSON encoder escapes each '<' as six bytes.
	want := store.InitialCodexFiles{
		AgentsMD:   strings.Repeat("<", maxInitialCodexFileBytes),
		ConfigTOML: runtime.CodexConfigSeed + "#" + strings.Repeat("<", maxInitialCodexFileBytes-len(runtime.CodexConfigSeed)-1),
	}
	var saved store.InitialCodexFiles
	api.call(http.MethodPut, codexFilesPath, want, http.StatusOK, &saved)
	if saved != want {
		t.Fatal("the saved files did not round-trip")
	}
}
