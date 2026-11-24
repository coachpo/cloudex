package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/coder/websocket"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const testOrigin = "http://localhost:3000"
const syntheticPassword = "test-admin-not-for-deployment"

type testAPI struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
}

func newTestAPI(t *testing.T, c *control.Control) testAPI {
	t.Helper()
	if e := c.Store.CreateAdmin(t.Context(), "admin", syntheticPassword, digest("test-admin-session"), time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	return newUninitializedTestAPI(t, c)
}

func newUninitializedTestAPI(t *testing.T, c *control.Control) testAPI {
	t.Helper()
	block, e := aes.NewCipher(make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	if c.Key, e = cipher.NewGCM(block); e != nil {
		t.Fatal(e)
	}
	h := (&Server{Control: c, Origin: testOrigin}).Handler(http.NotFoundHandler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return testAPI{t, srv, &http.Client{Jar: jar, Timeout: 25 * time.Second}}
}

func (a testAPI) raw(method, path, origin, key string, body any) *http.Response {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			a.t.Fatal(e)
		}
		reader = strings.NewReader(string(b))
	}
	r, e := http.NewRequest(method, a.server.URL+"/api/v1"+path, reader)
	if e != nil {
		a.t.Fatal(e)
	}
	r.Header.Set("Origin", origin)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	res, e := a.client.Do(r)
	if e != nil {
		a.t.Fatal(e)
	}
	return res
}

func (a testAPI) call(method, path string, body any, want int, dest any) {
	a.t.Helper()
	res := a.raw(method, path, testOrigin, "", body)
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		a.t.Fatal(e)
	}
	if res.StatusCode != want {
		a.t.Fatalf("%s %s: %d, expected %d: %s", method, path, res.StatusCode, want, raw)
	}
	if dest != nil {
		var data struct{ Data json.RawMessage }
		if e = json.Unmarshal(raw, &data); e != nil {
			a.t.Fatal(e)
		}
		if e = json.Unmarshal(data.Data, dest); e != nil {
			a.t.Fatalf("decode %s: %v: %s", path, e, raw)
		}
	}
}

func newDB(t *testing.T) *store.Store {
	t.Helper()
	s, e := store.Open(t.Context(), filepath.Join(t.TempDir(), "test.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (a testAPI) login() {
	a.t.Helper()
	a.call("POST", "/auth/login", map[string]string{"username": "admin", "password": syntheticPassword}, 200, nil)
}

func TestAdminSetupAndAccount(t *testing.T) {
	a := newUninitializedTestAPI(t, &control.Control{Store: newDB(t)})
	var setup struct{ Required bool }
	var session struct{ Username string }
	assertProblem := func(method, path string, body any, status int, code string) {
		t.Helper()
		res := a.raw(method, path, testOrigin, "", body)
		defer res.Body.Close()
		var payload struct{ Error struct{ Code string } }
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != status || payload.Error.Code != code {
			t.Fatalf("%s %s: got %d %s, want %d %s", method, path, res.StatusCode, payload.Error.Code, status, code)
		}
	}
	credentials := map[string]string{"username": "admin", "password": syntheticPassword}
	a.call("GET", "/auth/setup", nil, 200, &setup)
	if !setup.Required {
		t.Fatal("empty database does not require setup")
	}
	assertProblem("POST", "/auth/login", credentials, 409, "SETUP_REQUIRED")
	assertProblem("POST", "/auth/setup", map[string]string{"username": " admin", "password": syntheticPassword}, 400, "INVALID_USERNAME")
	assertProblem("POST", "/auth/setup", map[string]string{"username": "admin", "password": strings.Repeat("密", 25)}, 400, "INVALID_PASSWORD_LENGTH")
	foreign := a.raw("POST", "/auth/setup", "https://unrelated.example", "", credentials)
	foreign.Body.Close()
	if foreign.StatusCode != 403 {
		t.Fatalf("foreign origin setup: got HTTP %d", foreign.StatusCode)
	}
	a.call("POST", "/auth/setup", credentials, 200, &session)
	if session.Username != "admin" {
		t.Fatalf("setup returned username %q", session.Username)
	}
	session.Username = ""
	a.call("GET", "/auth/session", nil, 200, &session)
	if session.Username != "admin" {
		t.Fatalf("setup session has username %q", session.Username)
	}
	a.call("GET", "/auth/setup", nil, 200, &setup)
	if setup.Required {
		t.Fatal("setup is still required")
	}
	assertProblem("POST", "/auth/setup", credentials, 409, "SETUP_COMPLETED")

	other := a
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	other.client = &http.Client{Jar: jar, Timeout: a.client.Timeout}
	other.login()
	serverURL, err := url.Parse(a.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	originalCookies := a.client.Jar.Cookies(serverURL)
	const newPassword = "updated-synthetic-password"
	change := map[string]string{"username": "operator", "current_password": "wrong-password", "new_password": newPassword}
	assertProblem("PUT", "/auth/account", change, 403, "INVALID_PASSWORD")
	a.call("GET", "/auth/session", nil, 200, nil)
	other.call("GET", "/auth/session", nil, 200, nil)
	change["current_password"] = syntheticPassword
	a.call("PUT", "/auth/account", change, 200, &session)
	if session.Username != "operator" {
		t.Fatalf("account update returned username %q", session.Username)
	}
	session.Username = ""
	a.call("GET", "/auth/session", nil, 200, &session)
	if session.Username != "operator" {
		t.Fatalf("replacement session has username %q", session.Username)
	}
	other.call("GET", "/auth/session", nil, 401, nil)
	jar.SetCookies(serverURL, originalCookies)
	other.call("GET", "/auth/session", nil, 401, nil)
	a.call("POST", "/auth/login", credentials, 401, nil)
	a.call("POST", "/auth/login", map[string]string{"username": "operator", "password": syntheticPassword}, 401, nil)
	other.call("POST", "/auth/login", map[string]string{"username": "operator", "password": newPassword}, 200, nil)

	a.call("PUT", "/auth/account", map[string]string{"username": "renamed", "current_password": newPassword}, 200, nil)
	other.call("GET", "/auth/session", nil, 401, nil)
	other.call("POST", "/auth/login", map[string]string{"username": "renamed", "password": newPassword}, 200, nil)
}

func TestSessionAndOriginBoundaries(t *testing.T) {
	s := newDB(t)
	a := newTestAPI(t, &control.Control{Store: s})
	a.call("GET", "/workspaces", nil, 401, nil)
	req := a.raw("POST", "/auth/login", "https://unrelated.example", "", map[string]string{"username": "admin", "password": syntheticPassword})
	req.Body.Close()
	if req.StatusCode != 403 {
		t.Fatalf("foreign origin accepted: %d", req.StatusCode)
	}
	a.login()
	res := a.raw("GET", "/workspaces/placeholder/terminal", "https://unrelated.example", "", nil)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("foreign terminal origin accepted: %d", res.StatusCode)
	}
	a.call("POST", "/auth/logout", nil, 200, nil)
	a.call("GET", "/workspaces", nil, 401, nil)
}

func TestRepositoryCredentialsAreWriteOnly(t *testing.T) {
	ctx := t.Context()
	s := newDB(t)
	a := newTestAPI(t, &control.Control{Store: s})
	const firstToken = "synthetic-initial-credential"
	const secondToken = "synthetic-replacement-credential"
	const host = "git.example.test:8443"
	assertResponse := func(res *http.Response, status int, dest any) {
		t.Helper()
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != status {
			t.Fatalf("unexpected credential response status: %d, %v", res.StatusCode, err)
		}
		for _, token := range []string{firstToken, secondToken} {
			if strings.Contains(string(raw), token) {
				t.Fatal("credential token appeared in a response")
			}
		}
		if dest != nil {
			var envelope struct{ Data json.RawMessage }
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(envelope.Data, dest); err != nil {
				t.Fatal(err)
			}
		}
	}
	a.login()
	create := func(token string) acceptance {
		var got acceptance
		assertResponse(a.raw("POST", "/workspaces", testOrigin, "my-private-workspace", map[string]any{
			"name":       "private",
			"repository": map[string]any{"url": "https://" + host + "/example/repo", "credential": repositoryCredentialInput{Username: "git", Token: token}},
		}), 202, &got)
		return got
	}
	created := create(firstToken)
	if replay := create(secondToken); replay != created {
		t.Fatal("a retry with the same idempotency key and a different token did not return the original operation")
	}
	if saved, err := s.RepositoryCredential(ctx, created.Workspace); err != nil || saved.Host != host || saved.Token != firstToken {
		t.Fatalf("Create or its idempotent replay did not preserve the original host and token: %v", err)
	}
	op, err := s.Operation(ctx, created.Op)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishOp(ctx, op, "running", &store.Container{Generation: 1, ID: "container", Status: "running"}, ""); err != nil {
		t.Fatal(err)
	}
	workspace := "/workspaces/" + created.Workspace
	path := workspace + "/repository-credential"
	assertStatus := func(res *http.Response, configured bool, username string) {
		t.Helper()
		var status map[string]any
		assertResponse(res, 200, &status)
		if len(status) != 2 || status["configured"] != configured || status["username"] != username {
			t.Fatal("unexpected credential status shape or value")
		}
	}
	assertStatus(a.raw("GET", path, "", "", nil), true, "git")
	assertStatus(a.raw("PUT", path, testOrigin, "", repositoryCredentialInput{Username: "updated", Token: secondToken}), true, "updated")
	if saved, err := s.RepositoryCredential(ctx, created.Workspace); err != nil || saved.Host != host || saved.Username != "updated" || saved.Token != secondToken {
		t.Fatalf("PUT did not persist the replacement credential for the repository host: %v", err)
	}
	assertStatus(a.raw("GET", path, "", "", nil), true, "updated")
	assertResponse(a.raw("GET", workspace, "", "", nil), 200, nil)
	assertResponse(a.raw("GET", "/workspaces", "", "", nil), 200, nil)
	assertResponse(a.raw("DELETE", path, testOrigin, "", nil), 204, nil)
	assertStatus(a.raw("GET", path, "", "", nil), false, "")
	if _, err := s.RepositoryCredential(ctx, created.Workspace); !errors.Is(err, store.ErrMissing) {
		t.Fatalf("credential row remains after removal: %v", err)
	}
}

type acceptance struct {
	Workspace string `json:"workspace_id"`
	Op        string `json:"operation_id"`
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	until := time.Now().Add(20 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
func (a testAPI) opDone(id string) {
	a.t.Helper()
	waitFor(a.t, "operation "+id, func() bool {
		var o store.Operation
		a.call("GET", "/operations/"+id, nil, 200, &o)
		if o.Status == "failed" {
			a.t.Fatalf("%s failed: %s", o.Type, o.Error)
		}
		return o.Status == "succeeded"
	})
}
func (a testAPI) shell(path string, cols, rows int, command, marker string) string {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(a.t.Context(), 10*time.Second)
	defer cancel()
	u, _ := url.Parse(a.server.URL)
	h := http.Header{"Origin": {testOrigin}}
	for _, ck := range a.client.Jar.Cookies(u) {
		h.Add("Cookie", ck.String())
	}
	conn, res, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.server.URL, "http")+"/api/v1"+path+"/terminal", &websocket.DialOptions{HTTPHeader: h})
	if e != nil {
		a.t.Fatalf("terminal dial: %v (%v)", e, res)
	}
	defer conn.CloseNow()
	resize, _ := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	for _, m := range []struct {
		kind websocket.MessageType
		data []byte
	}{
		{websocket.MessageText, resize}, {websocket.MessageBinary, []byte(command + "\r")},
	} {
		if e = conn.Write(ctx, m.kind, m.data); e != nil {
			a.t.Fatal(e)
		}
	}
	var out strings.Builder
	for !strings.Contains(out.String(), marker) {
		_, b, e := conn.Read(ctx)
		if e != nil {
			a.t.Fatalf("terminal did not print %q: %v (got %q)", marker, e, out.String())
		}
		out.Write(b)
	}
	return out.String()
}

func TestDockerWorkspaceLifecycle(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_DOCKER") != "1" {
		t.Skip("set CLOUDEX_TEST_DOCKER=1 on a disposable Docker daemon with cloudex/workspace:local available")
	}
	ctx := t.Context()
	s := newDB(t)
	instance, e := s.Instance(ctx)
	if e != nil {
		t.Fatal(e)
	}
	d, e := runtime.NewDocker(instance)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	docker, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	networkName := "cloudex-lifecycle-" + instance[:12]
	bridge, err := docker.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := docker.NetworkRemove(cleanup, bridge.ID, client.NetworkRemoveOptions{}); err != nil {
			t.Error(err)
		}
	})
	c := &control.Control{Store: s, Engine: d, SSH: remote.Config{Network: networkName}}
	a := newTestAPI(t, c)
	a.login()
	run := func() func() {
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- c.Run(workerCtx) }()
		return func() {
			stop()
			if e := <-done; e != nil {
				t.Error(e)
			}
		}
	}
	stop := run()
	t.Cleanup(func() {
		if stop != nil {
			stop()
		}
	})
	create := func(name string) (string, string, runtime.Container) {
		t.Helper()
		var created acceptance
		a.call("POST", "/workspaces", map[string]any{"name": name, "resources": map[string]int{"cpu_milli": 500, "memory_mb": 512}}, 202, &created)
		path := "/workspaces/" + created.Workspace
		t.Cleanup(func() {
			end, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			for _, g := range []int{1, 2, 3, 4} {
				if e := d.Remove(end, runtime.Spec{WorkspaceID: created.Workspace, Generation: g}); e != nil {
					t.Error(e)
				}
			}
			if e := d.Purge(end, created.Workspace, true); e != nil {
				t.Error(e)
			}
		})
		a.opDone(created.Op)
		var info runtime.Container
		a.call("GET", path+"/inspect", nil, 200, &info)
		return created.Workspace, path, info
	}
	imageWorkspace, imagePath, imageInfo := create("image home")
	if !a.exec(d, imageInfo.ID, `test -w ~/.config && test -s ~/.bashrc &&
printf '%s\n' 'cli_auth_credentials_store = "file"' | cmp - ~/.codex/config.toml &&
test ! -e ~/.codex/auth.json`) {
		t.Fatal("the fresh home volume did not inherit the image's home files")
	}
	var op acceptance
	a.call("DELETE", imagePath, map[string]any{"confirm": imageWorkspace, "purge": true}, 202, &op)
	a.opDone(op.Op)

	settings := store.InitialCodexFiles{AgentsMD: "Initial workspace instructions\n",
		ConfigTOML: runtime.CodexConfigSeed + "# Initial text\nmodel = 'workspace-model'\n"}
	a.call("PUT", "/settings/codex-files", settings, 200, nil)
	workspace, path, info := create("acceptance")
	old := info.ID
	if info.State != "running" || len(info.Mounts) != 3 || old == "" {
		t.Fatalf("inspect: %+v", info)
	}
	a.call("GET", path+"/metrics", nil, 200, nil)
	a.call("GET", "/hosts/local/containers", nil, 200, nil)
	// Docker seeds the empty home volume before the initial files are copied in.
	if !a.exec(d, info.ID, `set -eu
test -s ~/.bashrc
printf '%s\n' 'Initial workspace instructions' | cmp - ~/.codex/AGENTS.md
printf '%s\n' 'cli_auth_credentials_store = "file"' '# Initial text' "model = 'workspace-model'" | cmp - ~/.codex/config.toml
test ! -e ~/.codex/auth.json
test "$(stat -c '%u:%g:%a' ~/.codex/AGENTS.md)" = 10001:10001:644
test "$(stat -c '%u:%g:%a' ~/.codex/config.toml)" = 10001:10001:600`) {
		t.Fatal("Create did not keep the image's home files and write the initial Codex files")
	}
	var homeVolume string
	for _, mount := range info.Mounts {
		if mount.Destination == "/home/coder" {
			homeVolume = mount.Name
		}
	}
	if homeVolume == "" {
		t.Fatal("Docker did not report the named home volume")
	}
	out := a.shell(path, 113, 37, `printf '%s' "$$" > terminal.pid; printf "kept\n" > keep.txt; printf "login-fixture" > ~/.codex/local-fixture; stty size; printf 'TERM%s\n' '_DONE'`, "TERM_DONE")
	if !strings.Contains(out, "37 113") {
		t.Fatalf("PTY size missing: %q", out)
	}
	waitFor(t, "terminal process to exit after disconnect", func() bool {
		return a.exec(d, info.ID, `test -s terminal.pid && ! kill -0 "$(cat terminal.pid)" 2>/dev/null`)
	})
	if !a.exec(d, info.ID, `set -eu
printf 'model = "manual"\n' > ~/.codex/config.toml
printf 'local instructions\n' > ~/.codex/AGENTS.md
printf '{"fixture":"synthetic-login"}\n' > ~/.codex/auth.json
mkdir -p ~/.local/bin
printf 'user tool' > ~/.local/bin/fixture-tool`) {
		t.Fatal("could not prepare local files in the named home volume")
	}
	a.call("PUT", "/settings/codex-files", store.InitialCodexFiles{
		AgentsMD: "Future Workspace instructions\n", ConfigTOML: "model = 'future-workspace'\n",
	}, 200, nil)
	checkLocalCodex := `set -eu
grep -Fxq 'model = "manual"' ~/.codex/config.toml
grep -Fxq 'local instructions' ~/.codex/AGENTS.md
grep -Fxq '{"fixture":"synthetic-login"}' ~/.codex/auth.json
grep -Fxq 'user tool' ~/.local/bin/fixture-tool
grep -Fxq 'login-fixture' ~/.codex/local-fixture`
	var activate acceptance
	a.call("POST", path+"/stop", nil, 202, &activate)
	a.opDone(activate.Op)
	a.call("POST", path+"/start", nil, 202, &activate)
	a.opDone(activate.Op)
	a.call("GET", path+"/inspect", nil, 200, &info)
	if info.ID != old || !a.exec(d, info.ID, checkLocalCodex) {
		t.Fatal("Stop and Start should preserve the original container and its home files")
	}

	if err := s.SetRepositoryCredential(ctx, workspace, &store.RepositoryCredential{
		Host: "git.example.test", Username: "fixture-user", Token: "synthetic-docker-git-token",
	}); err != nil {
		t.Fatal(err)
	}
	if !a.exec(d, info.ID, `test ! -e /etc/cloudex/git-credential.json`) {
		t.Fatal("saving the repository credential changed the existing container")
	}

	a.call("POST", path+"/rebuild", nil, 202, &activate)
	a.opDone(activate.Op)
	a.call("GET", path+"/inspect", nil, 200, &info)
	if info.ID == old || !a.exec(d, info.ID, checkLocalCodex) {
		t.Fatal("Rebuild did not preserve the home files")
	}
	if !a.exec(d, info.ID, `export GIT_TERMINAL_PROMPT=0
test "$(stat -c '%u:%g:%a' /etc/cloudex/git-credential.json)" = 10001:10001:600 || exit 1
matching=$(printf 'protocol=https\nhost=git.example.test\n\n' | git credential fill) || exit 1
printf '%s\n' "$matching" | grep -Fxq 'username=fixture-user' &&
printf '%s\n' "$matching" | grep -Fxq 'password=synthetic-docker-git-token' || exit 1
if unmatched=$(printf 'protocol=https\nhost=other.example.test\n\n' | git credential fill 2>/dev/null); then exit 1; fi
test -z "$unmatched"`) {
		t.Fatal("Git credential helper did not serve only the configured repository host")
	}
	if !a.exec(d, info.ID, `printf 'before\n' > tracked.txt && git add tracked.txt &&
git -c user.name=Fixture -c user.email=fixture@example.test -c commit.gpgsign=false commit -qm tracked &&
printf 'after\n' > tracked.txt`) {
		t.Fatal("could not set up tracked changes for the Git snapshot")
	}
	var snapshot store.Git
	a.call("GET", path+"/git", nil, 200, &snapshot)
	if snapshot.Head == "" || !strings.Contains(snapshot.Status, "?? keep.txt") ||
		!strings.Contains(snapshot.Patch, "+after") || strings.Contains(snapshot.Patch, "keep.txt") {
		t.Fatalf("Git snapshot did not distinguish tracked and untracked changes: %+v", snapshot)
	}

	old = info.ID

	if _, err := docker.ContainerPause(ctx, info.ID, client.ContainerPauseOptions{}); err != nil {
		t.Fatal(err)
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err = d.Run(probe, info.ID, runtime.Exec{Args: []string{"workspace-control", "git"}, Dir: runtime.WorkDir})
	cancel()
	if !errors.Is(err, runtime.ErrExecPaused) {
		t.Fatalf("Docker did not report a confirmed pause before Exec creation: %v", err)
	}
	waitFor(t, "paused workspace status", func() bool {
		var w store.Workspace
		a.call("GET", path, nil, 200, &w)
		return w.Paused && w.Observed == "stopped"
	})
	a.call("POST", path+"/start", nil, 409, nil)
	a.call("POST", path+"/stop", nil, 202, &op)
	a.opDone(op.Op)
	a.call("GET", path+"/inspect", nil, 200, &info)
	if info.State != "exited" {
		t.Fatalf("Stop finished without terminating the paused container: %s", info.State)
	}
	a.call("POST", path+"/start", nil, 202, &op)
	a.opDone(op.Op)

	if !a.exec(d, info.ID, checkLocalCodex+`
test "$(cat keep.txt)" = kept`) {
		t.Fatal("Rebuild did not preserve project and home files")
	}
	a.call("DELETE", path+"/repository-credential", nil, 204, nil)
	if !a.exec(d, info.ID, `test -f /etc/cloudex/git-credential.json`) {
		t.Fatal("clearing the repository credential changed the existing container")
	}

	old = info.ID
	a.call("POST", path+"/reset", nil, 400, nil)
	a.call("POST", path+"/reset", map[string]string{"confirm": workspace}, 202, &op)
	a.opDone(op.Op)
	a.call("GET", path+"/inspect", nil, 200, &info)
	if info.ID == old || !a.exec(d, info.ID, checkLocalCodex+"\ntest ! -e keep.txt && test ! -e /etc/cloudex/git-credential.json") {
		t.Fatal("Reset did not keep home files, recreate the project, or omit the cleared credential")
	}
	a.call("DELETE", path, map[string]any{"confirm": workspace, "purge": true}, 202, &op)
	a.opDone(op.Op)
	a.call("GET", path, nil, 404, nil)
	if _, err := docker.VolumeInspect(ctx, homeVolume, client.VolumeInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("Purge did not remove the home volume: %v", err)
	}
}

func (a testAPI) exec(engine *runtime.Docker, id, command string) bool {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(a.t.Context(), 5*time.Second)
	defer cancel()
	code, e := engine.Run(ctx, id, runtime.Exec{Args: []string{"sh", "-c", command}, Dir: runtime.WorkDir})
	return e == nil && code == 0
}

func (a testAPI) expectProblem(method, path string, body any, status int, code string) {
	a.t.Helper()
	res := a.raw(method, path, testOrigin, "", body)
	defer res.Body.Close()
	var response struct {
		Error struct{ Code, Message string }
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		a.t.Fatalf("%s %s: decode error response: %v", method, path, err)
	}
	if res.StatusCode != status || response.Error.Code != code || response.Error.Message == "" {
		a.t.Fatalf("%s %s: status=%d error=%+v, expected status=%d code=%s", method, path, res.StatusCode, response.Error, status, code)
	}
}
