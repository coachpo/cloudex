package httpapi

import (
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestWorkspaceCodexConfigAPI(t *testing.T) {
	db := newDB(t)
	workspace := seedWorkspace(t, db, "running", nil)
	api := newTestAPI(t, &control.Control{Store: db})
	path := "/workspaces/" + workspace.ID + "/codex-config"
	settings := store.CodexConfigSettings{
		Managed: true, AgentsMD: "# Project guidance\n", ConfigTOML: "# retained comment\n[profile]\nmodel = 'fixture'\n",
		AuthJSON: " {\n  \"fixture\": \"fake-login\"\n}\n",
	}
	request := map[string]any{
		"revision": 0, "managed": settings.Managed, "agents_md": settings.AgentsMD,
		"config_toml": settings.ConfigTOML, "auth_json": settings.AuthJSON,
	}
	api.call(http.MethodGet, path, nil, http.StatusUnauthorized, nil)
	api.call(http.MethodPut, path, request, http.StatusUnauthorized, nil)
	api.login()

	var config store.CodexConfig
	api.call(http.MethodGet, path, nil, http.StatusOK, &config)
	if config != (store.CodexConfig{CodexConfigSettings: store.CodexConfigSettings{ConfigTOML: runtime.CodexConfigSeed}}) {
		t.Fatalf("new workspace config: %+v", config)
	}
	request["sandbox_mode"] = "read-only"
	api.expectProblem(http.MethodPut, path, request, http.StatusBadRequest, "INVALID_REQUEST")
	delete(request, "sandbox_mode")

	want := store.CodexConfig{CodexConfigSettings: settings, Revision: 1}
	api.call(http.MethodPut, path, request, http.StatusOK, &config)
	if config != want {
		t.Fatalf("saved config: %+v", config)
	}
	var reloaded store.CodexConfig
	api.call(http.MethodGet, path, nil, http.StatusOK, &reloaded)
	if reloaded != want {
		t.Fatalf("read config: %+v", reloaded)
	}
	api.expectProblem(http.MethodPut, path, request, http.StatusConflict, "CODEX_CONFIG_CHANGED")
	for _, field := range []string{"agents_md", "config_toml", "auth_json"} {
		incomplete := maps.Clone(request)
		incomplete["revision"] = config.Revision
		incomplete["managed"] = false
		delete(incomplete, field)
		api.expectProblem(http.MethodPut, path, incomplete, http.StatusBadRequest, "INVALID_CODEX_CONFIG")
		incomplete[field] = nil
		api.expectProblem(http.MethodPut, path, incomplete, http.StatusBadRequest, "INVALID_CODEX_CONFIG")
	}

	request["revision"], request["config_toml"], request["auth_json"] = config.Revision, "", ""
	api.call(http.MethodPut, path, request, http.StatusOK, &config)
	api.call(http.MethodGet, path, nil, http.StatusOK, &reloaded)
	if config.Revision != 2 || config.ConfigTOML != "" || config.AuthJSON != "" || reloaded != config {
		t.Fatalf("empty files did not round trip: saved=%+v reloaded=%+v", config, reloaded)
	}
}

func TestWorkspaceCodexConfigRejectsInvalidFiles(t *testing.T) {
	db := newDB(t)
	workspace := seedWorkspace(t, db, "running", nil)
	api := newTestAPI(t, &control.Control{Store: db})
	api.login()
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"managed must be boolean", "managed", "true"},
		{"revision must be integer", "revision", 0.5},
		{"instructions must be a string", "agents_md", 42},
		{"TOML must be a string", "config_toml", map[string]any{}},
		{"auth must be a string", "auth_json", []string{}},
		{"instructions too large", "agents_md", strings.Repeat("a", maxCodexInstructionsBytes+1)},
		{"instructions with NUL", "agents_md", "# invalid\x00"},
		{"invalid TOML", "config_toml", "model ="},
		{"unsupported credential store", "config_toml", `cli_auth_credentials_store = "keyring"`},
		{"credential store must be a string", "config_toml", "cli_auth_credentials_store = true"},
		{"TOML too large", "config_toml", "#" + strings.Repeat("a", maxCodexTOMLBytes)},
		{"TOML with NUL", "config_toml", "# invalid\x00"},
		{"invalid auth JSON", "auth_json", "{invalid}"},
		{"auth JSON array", "auth_json", "[]"},
		{"auth JSON null", "auth_json", "null"},
		{"auth JSON string", "auth_json", `"{}"`},
		{"auth too large", "auth_json", `{"fixture":"` + strings.Repeat("a", maxCodexAuthBytes) + `"}`},
		{"auth with NUL", "auth_json", "{}\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := api
			api.t = t
			request := map[string]any{
				"revision": 0, "managed": true, "agents_md": "", "config_toml": runtime.CodexConfigSeed, "auth_json": "",
			}
			request[tc.field] = tc.value
			api.expectProblem(http.MethodPut, "/workspaces/"+workspace.ID+"/codex-config", request, http.StatusBadRequest, "INVALID_CODEX_CONFIG")
		})
	}
}

func TestWorkspaceCodexConfigAcceptsEscapedFilesAtLimits(t *testing.T) {
	db := newDB(t)
	workspace := seedWorkspace(t, db, "running", nil)
	api := newTestAPI(t, &control.Control{Store: db})
	api.login()
	// Go's JSON encoder escapes each '<' as six bytes.
	agents := strings.Repeat("<", maxCodexInstructionsBytes)
	config := runtime.CodexConfigSeed + "#" + strings.Repeat("<", maxCodexTOMLBytes-len(runtime.CodexConfigSeed)-1)
	auth := `{"fixture":"` + strings.Repeat("<", maxCodexAuthBytes-len(`{"fixture":""}`)) + `"}`
	var saved store.CodexConfig
	api.call(http.MethodPut, "/workspaces/"+workspace.ID+"/codex-config", map[string]any{
		"revision": 0, "managed": true, "agents_md": agents, "config_toml": config, "auth_json": auth,
	}, http.StatusOK, &saved)
	if saved.AgentsMD != agents || saved.ConfigTOML != config || saved.AuthJSON != auth {
		t.Fatal("the saved files did not round-trip")
	}
}
