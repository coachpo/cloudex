package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/httpapi"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/google/uuid"
)

type acceptanceCase struct {
	name, channel, expectedTransport string
	fault                            fixtureFault
	forceSSE, appServer, interactive bool
}

type acceptanceClient struct {
	bin, gatewayHost, image, version string
}

// The HTTP gateway, persistence, adapters and OAuth flows are the production
// paths. The native run stubs Host installation only. The Docker run confirms
// the real tmpfs installer and uses the workspace image's normal entrypoints.
// Every OpenAI-shaped endpoint is a loopback fixture.
func TestPinnedCodexGatewayMatrix(t *testing.T) {
	bin, dockerHost := os.Getenv("CLOUDEX_TEST_CODEX_BIN"), os.Getenv("CLOUDEX_TEST_GATEWAY_DNS")
	image, version := os.Getenv("CLOUDEX_TEST_WORKSPACE_IMAGE"), os.Getenv("CLOUDEX_TEST_CODEX_VERSION")
	if (image != "" || version != "") && (image == "" || version == "" || dockerHost == "") {
		t.Fatal("set CLOUDEX_TEST_WORKSPACE_IMAGE, CLOUDEX_TEST_CODEX_VERSION and CLOUDEX_TEST_GATEWAY_DNS together for a candidate image")
	}
	if dockerHost != "" && os.Getenv("CLOUDEX_TEST_DOCKER") != "1" {
		t.Fatal("set CLOUDEX_TEST_DOCKER=1 only with a disposable Docker daemon")
	}
	if bin == "" && dockerHost == "" {
		t.Skip("set CLOUDEX_TEST_CODEX_BIN to a pinned Codex binary, or use a disposable Docker daemon with CLOUDEX_TEST_DOCKER=1 and CLOUDEX_TEST_GATEWAY_DNS")
	}
	cases := []acceptanceCase{
		{name: "codex-ws", channel: codexupstream.ChannelCodex, expectedTransport: "ws"},
		{name: "codex-sse-426", channel: codexupstream.ChannelCodex, expectedTransport: "sse", fault: fixtureFault{refuseUpgrade: true}},
		{name: "chatgpt-ws", channel: codexupstream.ChannelChatGPT, expectedTransport: "ws"},
		{name: "chatgpt-sse-426", channel: codexupstream.ChannelChatGPT, expectedTransport: "sse", fault: fixtureFault{refuseUpgrade: true}},
		{name: "chatgpt-sse-route-not-supported", channel: codexupstream.ChannelChatGPT, expectedTransport: "sse", fault: fixtureFault{refuseUpgradeRoute: true}},
		{name: "codex-sse-forced", channel: codexupstream.ChannelCodex, expectedTransport: "sse", forceSSE: true},
		{name: "chatgpt-sse-forced", channel: codexupstream.ChannelChatGPT, expectedTransport: "sse", forceSSE: true},
		{name: "chatgpt-sse-missing-content-type", channel: codexupstream.ChannelChatGPT, expectedTransport: "sse", forceSSE: true, fault: fixtureFault{untypedSSE: true}},
		{name: "chatgpt-app-server", channel: codexupstream.ChannelChatGPT, expectedTransport: "ws", appServer: true},
		{name: "codex-stream-lost", channel: codexupstream.ChannelCodex, expectedTransport: "ws", fault: fixtureFault{dropStream: true}},
		{name: "chatgpt-terminal-lost", channel: codexupstream.ChannelChatGPT, expectedTransport: "ws", fault: fixtureFault{dropTerminal: true}},
	}
	if bin != "" {
		if !filepath.IsAbs(bin) {
			t.Fatal("CLOUDEX_TEST_CODEX_BIN must be absolute")
		}
		client := acceptanceClient{bin: bin, image: store.DefaultWorkspaceImage, version: codexupstream.CodexClientVersion}
		t.Run("native", func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) { runGatewayCase(t, tc, client) })
			}
		})
	}
	if dockerHost != "" {
		if image == "" {
			image, version = store.DefaultWorkspaceImage, codexupstream.CodexClientVersion
		}
		client := acceptanceClient{gatewayHost: dockerHost, image: image, version: version}
		t.Run("docker", func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) { runGatewayCase(t, tc, client) })
			}
			t.Run("codex-interactive", func(t *testing.T) {
				runGatewayCase(t, acceptanceCase{
					channel: codexupstream.ChannelCodex, expectedTransport: "ws", interactive: true,
				}, client)
			})
		})
	}
}

type acceptanceCommands struct {
	cwd, sandbox string
	make, tty    func(...string) *exec.Cmd
}

func runGatewayCase(t *testing.T, tc acceptanceCase, client acceptanceClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	base := t.TempDir()
	db, key := acceptanceStore(t, ctx, base)
	fake := newGatewayFixture(t, tc.channel, tc.fault)
	otherChannel := codexupstream.ChannelChatGPT
	if tc.channel == otherChannel {
		otherChannel = codexupstream.ChannelCodex
	}
	other := newGatewayFixture(t, otherChannel, fixtureFault{})
	tlsServer, origin, trust := makeAcceptanceTLS(t, client.gatewayHost)
	service, err := codexgateway.New(ctx, codexgateway.Config{
		Store: db, Key: key, Origin: origin, RedirectURI: "http://127.0.0.1:1455/auth/callback",
		Clients: []*codexupstream.Client{fake.adapter(), other.adapter()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	unknown := tc.fault.dropStream || tc.fault.dropTerminal
	var codexFallbacks atomic.Int32
	gateway := (&httpapi.Server{
		Control: &control.Control{Store: db, Key: key, Gateway: service},
		Origin:  origin, GrantGateway: service,
	}).Handler(http.NotFoundHandler())
	tlsServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unknown && r.Method == http.MethodPost && r.URL.Path == "/g/v1/responses" {
			codexFallbacks.Add(1)
		}
		gateway.ServeHTTP(w, r)
	})
	tlsServer.StartTLS()
	t.Cleanup(tlsServer.Close)
	workspace := store.Workspace{
		ID: store.ID(), Name: "gateway-acceptance", Image: client.image,
		Branch: "main", CPU: 500, Memory: 768, Desired: "running",
	}
	op, err := db.Create(ctx, workspace, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	const session = "synthetic-admin-session"
	start, err := service.BeginAuthorization(ctx, session, tc.channel, "")
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewServer(grantCallback(service, "127.0.0.1:1455"))
	t.Cleanup(callback.Close)
	finishFixtureCallback(t, ctx, fake, start.AuthorizationURL, callback.URL, "127.0.0.1:1455")
	grant, err := service.ConfirmAuthorization(ctx, session, start.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetBinding(ctx, workspace.ID, grant.ID, 0); err != nil {
		t.Fatal(err)
	}

	var commands acceptanceCommands
	if client.gatewayHost == "" {
		info := runtime.Container{ID: "synthetic-container", State: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		workspace = finishAcceptanceWorkspace(t, ctx, db, op, info)
		installed := &acceptanceCredential{}
		if err := service.ReconcileGateway(ctx, workspace, info, installed); err != nil {
			t.Fatal(err)
		}
		commands = prepareNativeCodex(t, ctx, installed.token, origin, trust, base, fake.model, client.bin, tlsServer.Client())
	} else {
		commands = prepareContainerCodex(t, ctx, db, op, workspace, service, trust, client.gatewayHost)
	}
	version, err := commands.make("--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli "+client.version {
		t.Fatalf("expected Codex CLI %s, got %q (%v)", client.version, version, err)
	}
	switch {
	case tc.appServer:
		runAcceptanceAppServer(t, commands, fake.model)
	case tc.interactive:
		runAcceptanceInteractive(t, commands)
	default:
		runAcceptanceExec(t, commands, tc.forceSSE, unknown)
	}
	catalogs, upgrades, titles, rounds := fake.snapshot()
	if other.requests.Load() != 0 {
		t.Fatal("the inactive authorization channel received an upstream request")
	}
	if catalogs == 0 || tc.forceSSE && upgrades != 0 || !tc.forceSSE && !tc.interactive && upgrades != 1 ||
		tc.interactive && (upgrades < 1 || upgrades > 2) ||
		titles > 1 || titles > 0 && !tc.interactive {
		t.Fatalf("catalog and Upgrade observations: catalog=%d Upgrade=%d title=%d", catalogs, upgrades, titles)
	}
	if unknown {
		if codexFallbacks.Load() != 1 || len(rounds) != 1 || rounds[0].Transport != "ws" || rounds[0].Terminal != "" {
			t.Fatalf("expected one blocked fallback and one upstream submission, got fallback=%d upstream=%+v", codexFallbacks.Load(), rounds)
		}
		t.Logf("%s %s → %s [%s]: one accepted generation, no terminal, Codex HTTPS fallback received 409 before leaving the gateway", tc.channel, rounds[0].Transport, rounds[0].Path, rounds[0].Identity)
		return
	}
	if len(rounds) != 3 {
		t.Fatalf("expected a generation and two dependent tool continuations, observed %+v", rounds)
	}
	expectedResults := []string{"", "call_fixture_1", "call_fixture_2"}
	for i, r := range rounds {
		if r.Transport != tc.expectedTransport || r.ToolResult != expectedResults[i] || r.Terminal != "response.completed" ||
			r.InputTokens != 10+i || r.OutputTokens != 5+i {
			t.Fatalf("unexpected submission %d: %+v", i+1, r)
		}
		t.Logf("%s %s %s identity=%s model=%s submission=%d previous_response_id=%q tool_result=%q terminal=%s usage=%d/%d",
			tc.channel, r.Transport, r.Path, r.Identity, r.Model, i+1, r.Previous, r.ToolResult, r.Terminal, r.InputTokens, r.OutputTokens)
	}
}

func acceptanceStore(t *testing.T, ctx context.Context, base string) (*store.Store, cipher.AEAD) {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(base, "acceptance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return db, key
}

func TestGrantGatewayLoopbackCallback(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_GATEWAY_LOOPBACK") != "1" {
		t.Skip("set CLOUDEX_TEST_GATEWAY_LOOPBACK=1 to use a registered callback port on loopback")
	}
	t.Run("direct", func(t *testing.T) { testGrantGatewayLoopbackCallback(t, false) })
	t.Run("ssh-forward", func(t *testing.T) { testGrantGatewayLoopbackCallback(t, true) })
}

func testGrantGatewayLoopbackCallback(t *testing.T, forward bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	receiver := httptest.NewUnstartedServer(nil)
	var listener net.Listener
	var err error
	for _, port := range []string{"1455", "1457"} {
		listener, err = net.Listen("tcp4", "127.0.0.1:"+port)
		if err == nil {
			break
		}
	}
	if listener == nil {
		t.Fatalf("both registered loopback ports are unavailable: %v", err)
	}
	defer listener.Close()
	expectedHost := listener.Addr().String()
	if !forward {
		_ = receiver.Listener.Close()
		receiver.Listener = listener
	}
	db, key := acceptanceStore(t, ctx, t.TempDir())
	codex := newGatewayFixture(t, codexupstream.ChannelCodex, fixtureFault{})
	chatGPT := newGatewayFixture(t, codexupstream.ChannelChatGPT, fixtureFault{})
	const session = "synthetic-admin-session"
	service, err := codexgateway.New(ctx, codexgateway.Config{
		Store: db, Key: key, Origin: "https://127.0.0.1:8443",
		RedirectURI: "http://" + expectedHost + "/auth/callback",
		Clients:     []*codexupstream.Client{codex.adapter(), chatGPT.adapter()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	receiver.Config.Handler = grantCallback(service, expectedHost)
	receiver.Start()
	defer receiver.Close()
	browserOrigin := receiver.URL
	var forwarded *atomic.Int32
	if forward {
		forwarded = callbackSSHForward(t, ctx, listener, receiver.Listener.Addr().(*net.TCPAddr).Port)
		browserOrigin = "http://" + expectedHost
	}
	for _, fake := range []*gatewayFixture{codex, chatGPT} {
		start, err := service.BeginAuthorization(ctx, session, fake.channel, "")
		if err != nil {
			t.Fatal(err)
		}
		finishFixtureCallback(t, ctx, fake, start.AuthorizationURL, browserOrigin, expectedHost)
		if _, err := service.ConfirmAuthorization(ctx, session, start.ID); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s callback reached %s and completed local identity confirmation", fake.channel, expectedHost)
	}
	if forward && forwarded.Load() != 2 {
		t.Fatalf("SSH forwarded %d of two callback connections", forwarded.Load())
	}
}

func finishFixtureCallback(t *testing.T, ctx context.Context, fake *gatewayFixture, authorizeURL, receiverOrigin, expectedHost string) {
	t.Helper()
	state, code, issued := fake.issueCode(authorizeURL)
	q := url.Values{"state": {state}, "code": {code}}
	if issued != "" {
		q.Set("client_id", issued)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, receiverOrigin+"/auth/callback?"+q.Encode(), nil)
	if err != nil {
		t.Fatal("could not construct local callback request")
	}
	req.Host = expectedHost
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	res, err := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		t.Fatal("local callback listener could not be reached")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Referrer-Policy") != "no-referrer" ||
		!bytes.Contains(copyFixtureBody(res.Body), []byte("Identity verified")) {
		t.Fatalf("loopback callback did not verify the selected identity (status %d)", res.StatusCode)
	}
}

func finishAcceptanceWorkspace(t *testing.T, ctx context.Context, db *store.Store, op store.Operation, info runtime.Container) store.Workspace {
	t.Helper()
	err := db.FinishOp(ctx, op, "running", &store.Container{
		Generation: op.Target, ID: info.ID, ImageID: info.ImageID, Status: "running",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := db.Workspace(ctx, op.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

type acceptanceCredential struct {
	token        string
	confirmation runtime.GatewayCredentialConfirmation
}

func (c *acceptanceCredential) InspectGatewayCredential(_ context.Context, _ runtime.Spec, _ runtime.GatewayInstance) (runtime.GatewayCredentialConfirmation, error) {
	return c.confirmation, nil
}

func (c *acceptanceCredential) InstallGatewayCredential(_ context.Context, _ runtime.Spec, instance runtime.GatewayInstance, install runtime.GatewayCredentialInstall) (runtime.GatewayCredentialConfirmation, error) {
	c.token = install.Token
	digest := sha256.Sum256([]byte(install.Token))
	c.confirmation = runtime.GatewayCredentialConfirmation{
		Digest: hex.EncodeToString(digest[:]), StartedAt: instance.StartedAt, Sequence: install.Sequence,
		Origin: install.Origin, BindingRevision: install.BindingRevision, GrantEpoch: install.GrantEpoch,
	}
	return c.confirmation, nil
}

func prepareNativeCodex(t *testing.T, ctx context.Context, token, origin string, trust []byte, base, model, bin string, client *http.Client) acceptanceCommands {
	t.Helper()
	envHome := filepath.Join(base, "codex-home")
	if err := os.Mkdir(envHome, 0700); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(base, "models.json")
	cert := filepath.Join(base, "gateway-ca.pem")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/g/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := copyFixtureBody(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(model)) {
		t.Fatalf("gateway catalog (%d) does not contain the bound model: %s", resp.StatusCode, body)
	}
	writeAcceptanceFile(t, catalog, body)
	writeAcceptanceFile(t, cert, trust)
	config := fmt.Sprintf(`model_provider = "cloudex"
model = %q
model_catalog_json = %q
check_for_update_on_startup = false
web_search = "disabled"
[analytics]
enabled = false
[model_providers.cloudex]
name = "Cloudex"
base_url = %q
env_key = "CLOUDEX_GATEWAY_TOKEN"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
request_max_retries = 0
stream_max_retries = 0
http_headers = { "X-Cloudex-Run-ID" = %q }
`, model, catalog, origin+"/g/v1", uuid.NewString())
	writeAcceptanceFile(t, filepath.Join(envHome, "config.toml"), []byte(config))
	env := append(acceptanceProxyEnv("127.0.0.1", cert),
		"HOME="+base, "CODEX_HOME="+envHome, "PATH=/usr/bin:/bin", "CLOUDEX_GATEWAY_TOKEN="+token,
	)
	return acceptanceCommands{cwd: base, sandbox: "read-only", make: func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env, cmd.Dir = env, base
		return cmd
	}}
}

func writeAcceptanceFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func acceptanceProxyEnv(gatewayHost, cert string) []string {
	const refusedProxy = "http://127.0.0.1:1"
	return []string{
		"HTTP_PROXY=" + refusedProxy, "HTTPS_PROXY=" + refusedProxy, "ALL_PROXY=" + refusedProxy,
		"NO_PROXY=localhost,127.0.0.1," + gatewayHost,
		"CODEX_CA_CERTIFICATE=" + cert, "SSL_CERT_FILE=" + cert, "CURL_CA_BUNDLE=" + cert,
		"DO_NOT_TRACK=1", "DISABLE_TELEMETRY=1",
	}
}

func prepareContainerCodex(t *testing.T, ctx context.Context, db *store.Store, op store.Operation, workspace store.Workspace, service *codexgateway.Service, trust []byte, gatewayHost string) acceptanceCommands {
	t.Helper()
	instance, err := db.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := runtime.NewDocker(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	spec := runtime.Spec{
		WorkspaceID: workspace.ID, Image: workspace.Image, CPU: workspace.CPU, Memory: workspace.Memory,
		CreationOperationID: op.ID, Generation: op.Target, ProjectVolumeRevision: 1,
	}
	info, err := engine.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := engine.Remove(cleanup, spec); err != nil {
			t.Error(err)
		}
		if err := engine.Purge(cleanup, spec, true); err != nil {
			t.Error(err)
		}
	})
	if err := engine.Start(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	info, err = engine.Inspect(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	workspace = finishAcceptanceWorkspace(t, ctx, db, op, info)
	if err := service.ReconcileGateway(ctx, workspace, info, engine); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cert := "/tmp/cloudex-acceptance-ca.pem"
	status, err := engine.Run(ctx, info.ID, runtime.Exec{
		Dir: "/workspace", Stdout: &output, Stderr: &output,
		Args: []string{"sh", "-c", `umask 077; printf '%s' "$1" > "$2" && workspace-control init "" main`, "cloudex-test", string(trust), cert},
	})
	if err != nil || status != 0 {
		t.Fatalf("prepare controlled workspace: status %d (%v)\n%s", status, err, output.String())
	}
	// This disposable Docker fixture supplies its own container boundary.
	// Its kernel does not support the client's nested Linux sandbox.
	dockerCommand := func(mode string, args ...string) *exec.Cmd {
		prefix := []string{"exec", mode, "--user", "coder", "--workdir", runtime.WorkDir, info.ID, "env"}
		prefix = append(prefix, acceptanceProxyEnv(gatewayHost, cert)...)
		if mode == "-t" {
			prefix = append(prefix, "TERM=xterm-256color", "sh", "-c", `stty rows 40 cols 120 && exec codex "$@"`, "cloudex-test")
		} else {
			prefix = append(prefix, "codex")
		}
		return exec.CommandContext(ctx, "docker", append(prefix, args...)...)
	}
	return acceptanceCommands{
		cwd: runtime.WorkDir, sandbox: "danger-full-access",
		make: func(args ...string) *exec.Cmd { return dockerCommand("-i", args...) },
		tty:  func(args ...string) *exec.Cmd { return dockerCommand("-t", args...) },
	}
}

func runAcceptanceExec(t *testing.T, commands acceptanceCommands, forceSSE, unknown bool) {
	t.Helper()
	var args []string
	if forceSSE {
		args = append(args, "-c", "model_providers.cloudex.supports_websockets=false")
	}
	args = append(args, "exec", "--json", "--ephemeral", "-s", commands.sandbox, "--skip-git-repo-check", "Call the fixture tools and report their result.")
	output, err := commands.make(args...).CombinedOutput()
	if unknown {
		if err == nil || bytes.Contains(output, []byte(`"type":"turn.completed"`)) ||
			!bytes.Contains(output, []byte("409 Conflict")) ||
			!bytes.Contains(output, []byte("This turn may already have been submitted.")) {
			t.Fatalf("Codex must report unknown submission after a stream loss: %v\n%s", err, output)
		}
		return
	}
	if err != nil || !bytes.Contains(output, []byte(fixtureSuccess)) || !bytes.Contains(output, []byte(`"type":"turn.completed"`)) {
		t.Fatalf("Codex did not complete the fixture turn: %v\n%s", err, output)
	}
	var fields struct {
		Type  string
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
	}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		fields.Type = ""
		if json.Unmarshal(line, &fields) == nil && fields.Type == "turn.completed" {
			if fields.Usage.InputTokens != 33 || fields.Usage.OutputTokens != 18 {
				t.Fatalf("client-observed usage = %d/%d, expected 33/18", fields.Usage.InputTokens, fields.Usage.OutputTokens)
			}
			return
		}
	}
	t.Fatal("Codex did not emit a parseable terminal event")
}

func runAcceptanceInteractive(t *testing.T, commands acceptanceCommands) {
	t.Helper()
	cmd := commands.tty("--no-daemon", "--no-alt-screen", "-s", commands.sandbox, "-a", "never",
		"-c", "check_for_update_on_startup=false",
		"-c", fmt.Sprintf("projects={ %q = { trust_level = \"trusted\" } }", commands.cwd),
		"Call the fixture tools and report their result.")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	var output bytes.Buffer
	chunk := make([]byte, 4096)
	for {
		size, err := stdout.Read(chunk)
		output.Write(chunk[:size])
		if bytes.Contains(output.Bytes(), []byte(fixtureSuccess)) {
			return
		}
		if err != nil || output.Len() > 256<<10 {
			t.Fatalf("interactive Codex did not render its final result: %v\n%s", err, output.Bytes())
		}
	}
}

func runAcceptanceAppServer(t *testing.T, commands acceptanceCommands, model string) {
	t.Helper()
	cmd := commands.make("app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	in := json.NewEncoder(stdin)
	out := bufio.NewScanner(stdout)
	out.Buffer(make([]byte, 65536), 4<<20)
	send := func(id int, method string, params any) {
		event := map[string]any{"method": method}
		if id != 0 {
			event["id"] = id
		}
		if params != nil {
			event["params"] = params
		}
		if err := in.Encode(event); err != nil {
			t.Fatalf("app-server %s: %v", method, err)
		}
	}
	type reply struct {
		ID                    int
		Method                string
		Result, Error, Params json.RawMessage
	}
	next := func() reply {
		if !out.Scan() {
			_ = cmd.Wait()
			t.Fatalf("app-server stream closed: %v\n%s", out.Err(), stderr.String())
		}
		var e reply
		if err := json.Unmarshal(out.Bytes(), &e); err != nil {
			t.Fatalf("app-server invalid JSON-RPC: %v", err)
		}
		return e
	}
	wait := func(id int) json.RawMessage {
		for {
			e := next()
			if e.ID == id {
				if len(e.Error) != 0 {
					t.Fatalf("app-server request %d: %s", id, e.Error)
				}
				return e.Result
			}
		}
	}
	send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "cloudex_test", "version": "1.0.0"}})
	wait(1)
	send(0, "initialized", nil)
	send(2, "thread/start", map[string]any{"model": model, "cwd": commands.cwd, "approvalPolicy": "never", "sandbox": commands.sandbox})
	var started struct{ Thread struct{ ID string } }
	if err := json.Unmarshal(wait(2), &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("app-server did not provide a thread: %v", err)
	}
	send(3, "turn/start", map[string]any{
		"threadId": started.Thread.ID,
		"input":    []map[string]any{{"type": "text", "text": "Call the fixture tools and report their result.", "text_elements": []any{}}},
	})
	message := false
	for {
		e := next()
		if e.ID == 3 && len(e.Error) != 0 {
			t.Fatalf("app-server turn failed to start: %s", e.Error)
		}
		if e.Method == "item/completed" && bytes.Contains(e.Params, []byte(fixtureSuccess)) {
			message = true
		}
		if e.Method == "turn/completed" {
			var status struct{ Turn struct{ Status string } }
			if err := json.Unmarshal(e.Params, &status); err != nil || status.Turn.Status != "completed" || !message {
				t.Fatalf("app-server completed without the expected result: %s", e.Params)
			}
			return
		}
	}
}

func makeAcceptanceTLS(t *testing.T, gatewayHost string) (*httptest.Server, string, []byte) {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	if gatewayHost != "" {
		_ = server.Listener.Close()
		l, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		server.Listener = l
	} else {
		gatewayHost = "127.0.0.1"
	}
	port := server.Listener.Addr().(*net.TCPAddr).Port
	origin := fmt.Sprintf("https://%s", net.JoinHostPort(gatewayHost, fmt.Sprint(port)))
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cloudex controlled test CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, &root, &root, pk, sk)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leafPublic, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: gatewayHost},
		NotBefore: root.NotBefore, NotAfter: root.NotAfter,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(gatewayHost); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = []string{gatewayHost}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &leaf, issuer, leafPublic, sk)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: leafKey}}}
	return server, origin, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}
