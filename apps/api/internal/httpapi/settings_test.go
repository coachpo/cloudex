package httpapi

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestConsoleSettingsDefaultsSaveAndClear(t *testing.T) {
	a := newTestAPI(t, &control.Control{
		Store: newDB(t),
		SSH:   remote.Config{Bind: netip.MustParseAddr("127.0.0.1")},
	})
	const accessPath = "/settings/access"
	a.call(http.MethodPut, accessPath, map[string]string{"ssh_host": "", "agent_public_address": ""}, 401, nil)
	a.login()

	assertSettings := func(sshHost, agentAddress string) {
		t.Helper()
		var got map[string]any
		a.call(http.MethodGet, "/settings", nil, 200, &got)
		want := map[string]any{"workspace_image": store.DefaultWorkspaceImage, "ssh_bind": "127.0.0.1",
			"ssh_host": sshHost, "agent_public_address": agentAddress, "codex_config_toml": "", "codex_agents_md": ""}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("settings = %+v; want %+v", got, want)
		}
	}
	assertAccess := func(sshHost, agentAddress string) {
		t.Helper()
		want := map[string]string{"ssh_host": sshHost, "agent_public_address": agentAddress}
		var got map[string]string
		a.call(http.MethodPut, accessPath, want, 200, &got)
		if !maps.Equal(got, want) {
			t.Fatalf("access settings = %+v; want %+v", got, want)
		}
	}
	assertSettings("", "")
	assertAccess("gateway.example.org", "[2001:db8::1]:65535")
	assertSettings("gateway.example.org", "[2001:db8::1]:65535")
	assertAccess("", "")
	assertSettings("", "")
}

func TestCodexSettingsSaveValidateAndClear(t *testing.T) {
	a := newTestAPI(t, &control.Control{Store: newDB(t)})
	const path = "/settings/codex"
	const maxFileBytes = 32 * 1024
	a.call(http.MethodPut, path, store.CodexSettings{}, http.StatusUnauthorized, nil)
	a.login()
	assertStored := func(want store.CodexSettings) {
		t.Helper()
		var stored struct {
			ConfigTOML string `json:"codex_config_toml"`
			AgentsMD   string `json:"codex_agents_md"`
		}
		a.call(http.MethodGet, "/settings", nil, http.StatusOK, &stored)
		if stored.ConfigTOML != want.ConfigTOML || stored.AgentsMD != want.AgentsMD {
			t.Fatal("GET /settings did not return the saved Codex settings verbatim")
		}
	}
	save := func(settings store.CodexSettings) {
		t.Helper()
		var saved store.CodexSettings
		a.call(http.MethodPut, path, settings, http.StatusOK, &saved)
		if saved != settings {
			t.Fatal("PUT /settings/codex did not return the saved files verbatim")
		}
		assertStored(settings)
	}
	assertStored(store.CodexSettings{})
	const config = `# Workspace defaults (literal \e)
model = "gpt-6.2-sol"
model_context_window = 120000
[mcp_servers.fixture]
command = "fixture"
args = ['\e', "\\e", "\u001b", "0000-01-01"]
[shell_environment_policy]
exclude = ["FIXTURE_TOKEN"]
[projects."/workspace/project"]
trust_level = "trusted"
`
	saved := store.CodexSettings{ConfigTOML: config, AgentsMD: "# Instructions\r\n\r\nKeep trailing space. \n"}
	save(saved)
	assertInvalid := func(t *testing.T, body any, location string) {
		t.Helper()
		res := a.raw(http.MethodPut, path, testOrigin, "", body)
		defer res.Body.Close()
		var result struct {
			Error struct{ Code, Message string }
		}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusBadRequest || result.Error.Code != "INVALID_CODEX_SETTINGS" ||
			!strings.Contains(result.Error.Message, location) {
			t.Fatalf("status=%d error=%+v, want 400 INVALID_CODEX_SETTINGS identifying %q", res.StatusCode, result.Error, location)
		}
	}
	for _, tc := range []struct {
		name, location string
		body           any
	}{
		{"missing config", "config_toml", map[string]string{"agents_md": ""}},
		{"missing instructions", "agents_md", map[string]string{"config_toml": ""}},
		{"null config", "config_toml", map[string]any{"config_toml": nil, "agents_md": ""}},
		{"nonstring instructions", "agents_md", map[string]any{"config_toml": "", "agents_md": true}},
		{"invalid TOML", "config.toml", store.CodexSettings{ConfigTOML: "[mcp_servers.fixture"}},
		{"TOML 1.1 escape", "config.toml", store.CodexSettings{ConfigTOML: `custom = "\e"`}},
		{"TOML 1.1 quoted key", "config.toml", store.CodexSettings{ConfigTOML: `["\e"]`}},
		{"TOML 1.1 nested escape", "config.toml", store.CodexSettings{ConfigTOML: `custom = [{key = """\e"""}]`}},
		{"local date year zero", "config.toml", store.CodexSettings{ConfigTOML: "custom = 0000-01-01"}},
		{"local datetime year zero", "config.toml", store.CodexSettings{ConfigTOML: "custom = [0000-01-01T01:02:03]"}},
		{"offset datetime year zero", "config.toml", store.CodexSettings{ConfigTOML: "custom = {date = 0000-01-01T01:02:03Z}"}},
		{"oversize config", "config.toml", store.CodexSettings{ConfigTOML: "#" + strings.Repeat("a", maxFileBytes)}},
		{"oversize instructions in bytes", "AGENTS.md", store.CodexSettings{AgentsMD: strings.Repeat("界", maxFileBytes/3+1)}},
		{"oversize JSON request", "config.toml and AGENTS.md", store.CodexSettings{ConfigTOML: "#" + strings.Repeat("a", 500*1024)}},
		{"NUL in config", "config.toml", store.CodexSettings{ConfigTOML: "#\x00"}},
		{"NUL in instructions", "AGENTS.md", store.CodexSettings{AgentsMD: "\x00"}},
		{"model provider", "model_provider", store.CodexSettings{ConfigTOML: `model_provider = "cloudex"`}},
		{"model providers table", "model_providers", store.CodexSettings{ConfigTOML: "[model_providers.cloudex]"}},
		{"model catalog", "model_catalog_json", store.CodexSettings{ConfigTOML: `model_catalog_json = ""`}},
		{"auth store", "cli_auth_credentials_store", store.CodexSettings{ConfigTOML: `cli_auth_credentials_store = "file"`}},
		{"OpenAI base URL", "openai_base_url", store.CodexSettings{ConfigTOML: `"openai_base_url" = ""`}},
		{"ChatGPT base URL", "chatgpt_base_url", store.CodexSettings{ConfigTOML: `chatgpt_base_url = ""`}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertInvalid(t, tc.body, tc.location) })
	}
	assertStored(saved)
	save(store.CodexSettings{ConfigTOML: "#" + strings.Repeat("\t", maxFileBytes-1), AgentsMD: strings.Repeat("\x01", maxFileBytes)})
	save(store.CodexSettings{})
}

func TestWorkspaceImageSettingsDefaultSaveAndRejectInvalid(t *testing.T) {
	a := newTestAPI(t, &control.Control{Store: newDB(t)})
	const path = "/settings/workspace-image"
	a.call(http.MethodPut, path, map[string]string{"image": "busybox:V2"}, http.StatusUnauthorized, nil)
	a.login()

	assertImage := func(want string) {
		t.Helper()
		var got map[string]any
		a.call(http.MethodGet, "/settings", nil, http.StatusOK, &got)
		if got["workspace_image"] != want {
			t.Fatalf("workspace_image = %q; want %q", got["workspace_image"], want)
		}
	}
	assertImage(store.DefaultWorkspaceImage)
	var saved map[string]string
	a.call(http.MethodPut, path, map[string]string{"image": " \tbusybox:V2\n"}, http.StatusOK, &saved)
	if saved["workspace_image"] != "busybox:V2" {
		t.Fatalf("saved workspace_image = %q; want the trimmed reference", saved["workspace_image"])
	}
	assertImage("busybox:V2")

	for _, image := range []string{" ", "https://registry.example.org/workspace:latest", "registry.example.org/Workspace:latest", "workspace@sha256:123"} {
		a.expectProblem(http.MethodPut, path, map[string]string{"image": image}, http.StatusBadRequest, "INVALID_WORKSPACE_IMAGE")
	}
	a.expectProblem(http.MethodPut, path, map[string]any{}, http.StatusBadRequest, "INVALID_WORKSPACE_IMAGE")
	assertImage("busybox:V2")
}

func TestWorkspaceTemplatesAndCreationUseLatestImage(t *testing.T) {
	db := newDB(t)
	ctx := t.Context()
	const serial = "workspace-image-test"
	const replaced = "registry.example.org/workspace:v2"
	if err := db.SetLocalHostCertificate(ctx, serial); err != nil {
		t.Fatal(err)
	}
	if err := db.Heartbeat(ctx, store.LocalHostID, serial, store.HostReport{
		Healthy: true, MaxOperations: 1, CPU: 6000, Memory: 12 << 30,
		Images: []store.ImageStatus{
			{Image: store.DefaultWorkspaceImage, Ready: true},
			{Image: replaced, Ready: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	a := newTestAPI(t, &control.Control{Store: db})
	a.login()

	assertTemplate := func(want string) {
		t.Helper()
		var templates []struct{ Image string }
		a.call(http.MethodGet, "/templates", nil, http.StatusOK, &templates)
		if len(templates) != 1 || templates[0].Image != want {
			t.Fatalf("templates = %+v; want one template with image %q", templates, want)
		}
	}
	create := func(name, key string) acceptance {
		t.Helper()
		res := a.raw(http.MethodPost, "/workspaces", testOrigin, key, map[string]any{
			"name": name, "placement": map[string]string{"type": "manual", "host_id": store.LocalHostID},
		})
		defer res.Body.Close()
		var result struct {
			Data  acceptance
			Error json.RawMessage
		}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusAccepted || result.Data.Workspace == "" || result.Data.Op == "" {
			t.Fatalf("create workspace %q: status=%d data=%+v error=%s", name, res.StatusCode, result.Data, result.Error)
		}
		return result.Data
	}
	assertWorkspaceImage := func(id, want string) {
		t.Helper()
		var workspace store.Workspace
		a.call(http.MethodGet, "/workspaces/"+id, nil, http.StatusOK, &workspace)
		if workspace.Image != want {
			t.Fatalf("Workspace %s image = %q; want %q", id, workspace.Image, want)
		}
	}

	assertTemplate(store.DefaultWorkspaceImage)
	first := create("initial workspace", "initial-create")
	a.call(http.MethodPut, "/settings/workspace-image", map[string]string{"image": replaced}, http.StatusOK, nil)
	assertTemplate(replaced)
	second := create("new workspace", "new-create")
	assertWorkspaceImage(first.Workspace, store.DefaultWorkspaceImage)
	assertWorkspaceImage(second.Workspace, replaced)
	if replayed := create("initial workspace", "initial-create"); replayed != first {
		t.Fatalf("idempotent create after updating the default returned %+v; want %+v", replayed, first)
	}
}

func TestConsoleAccessSettingsRejectInvalidAddresses(t *testing.T) {
	db := newDB(t)
	a := newTestAPI(t, &control.Control{Store: db})
	a.login()
	const path = "/settings/access"
	const savedSSH, savedAgent = "gateway.example.org", "192.0.2.1:9443"
	a.call(http.MethodPut, path, map[string]string{"ssh_host": savedSSH, "agent_public_address": savedAgent}, 200, nil)

	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"SSH hostname injection", map[string]any{"ssh_host": "host\nProxyCommand", "agent_public_address": ""}, "INVALID_SSH_HOST"},
		{"SSH hostname with port", map[string]any{"ssh_host": "host:2222", "agent_public_address": ""}, "INVALID_SSH_HOST"},
		{"missing SSH hostname", map[string]any{"agent_public_address": ""}, "INVALID_REQUEST"},
		{"missing Agent address", map[string]any{"ssh_host": ""}, "INVALID_REQUEST"},
		{"null SSH hostname", map[string]any{"ssh_host": nil, "agent_public_address": ""}, "INVALID_REQUEST"},
		{"null Agent address", map[string]any{"ssh_host": "", "agent_public_address": nil}, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := a
			a.t = t
			a.expectProblem(http.MethodPut, path, tc.body, 400, tc.code)
		})
	}
	for _, addr := range []string{"agent.example.org", "agent.example.org:0", "agent.example.org:65536",
		"agent.example.org:+22", "agent.example.org:ssh", "bad_host:22", ":22", "https://agent.example.org:22",
		"2001:db8::1:22", "[agent.example.org]:22", "agent.example.org:22\n",
		"999.999.999.999:9443", "127.0.0.1.:9443"} {
		t.Run(addr, func(t *testing.T) {
			a := a
			a.t = t
			a.expectProblem(http.MethodPut, path, map[string]string{"ssh_host": "", "agent_public_address": addr}, 400, "INVALID_AGENT_PUBLIC_ADDRESS")
		})
	}
	if got, err := db.Settings(t.Context()); err != nil || got.SSHHost != savedSSH || got.AgentPublicAddress != savedAgent {
		t.Fatalf("invalid address update changed stored settings: %+v %v", got, err)
	}
}

func TestHostEnrollmentUsesCurrentAgentPublicAddress(t *testing.T) {
	db := newDB(t)
	if err := db.CreateAdmin(t.Context(), "admin", syntheticPassword, digest("settings-enrollment-fixture"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := &Server{Control: &control.Control{Store: db}, Origin: testOrigin, AgentCertificate: "test CA"}
	srv := httptest.NewServer(s.Handler(http.NotFoundHandler()))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	a := testAPI{t, srv, &http.Client{Jar: jar, Timeout: 25 * time.Second}}
	a.login()

	type enrollment struct {
		Host         store.Host
		AgentAddress *string `json:"agent_address"`
	}
	var created enrollment
	a.call(http.MethodPost, "/hosts", map[string]string{"name": "test host"}, 201, &created)
	if created.AgentAddress == nil || *created.AgentAddress != "" {
		t.Fatalf("default enrollment address = %v; want empty", created.AgentAddress)
	}
	for _, addr := range []string{"agent.example.org:9443", ""} {
		a.call(http.MethodPut, "/settings/access", map[string]string{"ssh_host": "", "agent_public_address": addr}, 200, nil)
		var renewed enrollment
		a.call(http.MethodPost, "/hosts/"+created.Host.ID+"/enrollment", nil, 200, &renewed)
		if renewed.AgentAddress == nil || *renewed.AgentAddress != addr {
			t.Fatalf("renewed enrollment address for %q: %+v", addr, renewed)
		}
	}
}
