package httpapi

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"
)

func testSSHKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, p, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ssh.NewSignerFromKey(p)
	if e != nil {
		t.Fatal(e)
	}
	return s, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

func sshClient(addr, user string, host ssh.PublicKey, auth ...ssh.AuthMethod) (*ssh.Client, error) {
	conn, e := net.DialTimeout("tcp", addr, 3*time.Second)
	if e != nil {
		return nil, e
	}
	return wrapSSH(conn, addr, user, host, auth...)
}

func wrapSSH(conn net.Conn, addr, user string, host ssh.PublicKey, auth ...ssh.AuthMethod) (*ssh.Client, error) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	c, channels, requests, e := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{User: user, Auth: auth, HostKeyCallback: ssh.FixedHostKey(host)})
	if e != nil {
		conn.Close()
		return nil, e
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, channels, requests), nil
}

func TestDockerWorkspaceRemoteSSH(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_SSH") != "1" {
		t.Skip("set CLOUDEX_TEST_SSH=1 and CLOUDEX_TEST_SSH_NETWORK on a disposable, directly reachable Docker bridge")
	}
	ctx := t.Context()
	db := newDB(t)
	instance, e := db.Instance(ctx)
	if e != nil {
		t.Fatal(e)
	}
	d, e := runtime.NewDocker(instance)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close() })
	conf, e := remote.NewConfig("127.0.0.1", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), os.Getenv("CLOUDEX_TEST_SSH_NETWORK"))
	if e != nil {
		t.Fatal(e)
	}
	c := &control.Control{Store: db, Engine: d, SSH: conf}
	a := newTestAPI(t, c)
	a.login()
	signer, e := c.PrepareGateway(ctx, instance)
	if e != nil {
		t.Fatal(e)
	}
	again, e := c.PrepareGateway(ctx, instance)
	if e != nil || ssh.FingerprintSHA256(signer.PublicKey()) != ssh.FingerprintSHA256(again.PublicKey()) {
		t.Fatalf("gateway identity did not persist: %v", e)
	}
	started, cancel := context.WithCancel(ctx)
	worker := make(chan error, 1)
	gateway := make(chan error, 1)
	go func() { worker <- c.Run(started) }()
	go func() { gateway <- remote.Serve(started, listener, signer, c) }()
	t.Cleanup(func() {
		cancel()
		if e := <-worker; e != nil {
			t.Error(e)
		}
		if e := <-gateway; e != nil {
			t.Error(e)
		}
	})
	var created, op acceptance
	a.call("POST", "/workspaces", map[string]any{"name": "ssh acceptance", "resources": map[string]int{"cpu_milli": 500, "memory_mb": 512}}, 202, &created)
	path := "/workspaces/" + created.Workspace
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for gen := 1; gen <= 6; gen++ {
			if e := d.Remove(cleanup, runtime.Spec{WorkspaceID: created.Workspace, Generation: gen}); e != nil {
				t.Error(e)
			}
		}
		if e := d.Purge(cleanup, created.Workspace, true); e != nil {
			t.Error(e)
		}
	})
	a.opDone(created.Op)
	get := func(status string) control.Access {
		t.Helper()
		var v control.Access
		a.call("GET", path+"/access", nil, 200, &v)
		if v.Status != status {
			t.Fatalf("SSH status: %+v, want %s", v, status)
		}
		return v
	}
	get("disabled")
	a.call("PUT", path+"/access", map[string]any{"public_keys": []string{"not a key"}}, 400, nil)
	first, line := testSSHKey(t)
	rotate := func(keys ...string) {
		t.Helper()
		if keys == nil {
			keys = []string{}
		}
		a.call("PUT", path+"/access", map[string]any{"public_keys": keys}, 202, &op)
		a.opDone(op.Op)
	}
	rotate(line)
	ready := get("ready")
	if ready.Port != conf.Port || !strings.Contains(ready.SSHConfig, "ProxyJump "+ready.Alias+"-gateway") || ready.ProjectPath != "/workspace/project" {
		t.Fatalf("connection metadata: %+v", ready)
	}
	_, jumpHosts, jumpKey, _, rest, e := ssh.ParseKnownHosts([]byte(ready.KnownHosts))
	if e != nil || len(jumpHosts) != 1 || jumpHosts[0] != c.GatewayKeyAlias || ready.GatewayFingerprint != ssh.FingerprintSHA256(jumpKey) {
		t.Fatalf("gateway known_hosts mismatch: %v", e)
	}
	_, hosts, hostKey, _, _, e := ssh.ParseKnownHosts(rest)
	if e != nil || len(hosts) != 1 || hosts[0] != ready.Alias || ready.Fingerprint != ssh.FingerprintSHA256(hostKey) {
		t.Fatalf("known_hosts mismatch: %v", e)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", ready.Port)
	backend := net.JoinHostPort(ready.Alias, strconv.Itoa(remote.BackendPort))
	dial := func(key ssh.Signer, user string, auth ...ssh.AuthMethod) (*ssh.Client, error) {
		jump, e := sshClient(addr, ready.Alias, jumpKey, ssh.PublicKeys(key))
		if e != nil {
			return nil, e
		}
		socket, e := jump.Dial("tcp", backend)
		if e != nil {
			jump.Close()
			return nil, e
		}
		endpoint, e := wrapSSH(socket, backend, user, hostKey, auth...)
		if e != nil {
			jump.Close()
			return nil, e
		}
		t.Cleanup(func() { endpoint.Close(); jump.Close() })
		return endpoint, nil
	}
	login := func(key ssh.Signer) *ssh.Client {
		t.Helper()
		conn, e := dial(key, "coder", ssh.PublicKeys(key))
		if e != nil {
			t.Fatal(e)
		}
		return conn
	}
	rejects := func(key ssh.Signer, user string, auth ...ssh.AuthMethod) {
		t.Helper()
		if conn, e := dial(key, user, auth...); e == nil {
			conn.Close()
			t.Fatalf("unexpected SSH access as %s", user)
		}
	}
	jump, e := sshClient(addr, ready.Alias, jumpKey, ssh.PublicKeys(first))
	if e != nil {
		t.Fatal(e)
	}
	defer jump.Close()
	for _, target := range []string{"localhost:2222", ready.Alias + ":22", "cloudex-" + strings.Repeat("a", 32) + ":2222"} {
		if socket, e := jump.Dial("tcp", target); e == nil {
			socket.Close()
			t.Fatalf("gateway allowed other destination %s", target)
		}
	}
	if session, e := jump.NewSession(); e == nil {
		session.Close()
		t.Fatal("gateway exposed a shell")
	}
	w, e := db.Workspace(ctx, created.Workspace)
	if e != nil {
		t.Fatal(e)
	}
	info, e := c.Inspect(ctx, w)
	if e != nil || !info.NetworkIPs[conf.Network].IsValid() {
		t.Fatalf("workspace bridge address unavailable: %+v %v", info, e)
	}
	docker, e := client.New(client.FromEnv)
	if e != nil {
		t.Fatal(e)
	}
	defer docker.Close()
	inspected, e := docker.ContainerInspect(ctx, info.ID, client.ContainerInspectOptions{})
	if e != nil {
		t.Fatal(e)
	}
	container := inspected.Container
	if container.HostConfig == nil || container.NetworkSettings == nil {
		t.Fatal("Docker did not report workspace port configuration")
	}
	port := network.MustParsePort(strconv.Itoa(remote.BackendPort) + "/tcp")
	if len(container.HostConfig.PortBindings[port]) != 0 || len(container.NetworkSettings.Ports[port]) != 0 {
		t.Fatalf("workspace SSH port %s is published on the Docker host", port)
	}
	a.call("POST", path+"/stop", nil, 202, &op)
	a.opDone(op.Op)
	get("stopped")
	rejects(first, "coder", ssh.PublicKeys(first))
	a.call("PUT", path+"/codex-config", map[string]any{
		"revision": 0, "managed": true, "agents_md": "Managed SSH instructions\n",
		"config_toml": runtime.CodexConfigSeed + "model = 'ssh-fixture-model'\n",
		"auth_json":   `{"fixture":"fake-ssh-login"}`,
	}, 200, nil)
	a.call("POST", path+"/start", nil, 202, &op)
	a.opDone(op.Op)
	if v := get("ready"); v.Fingerprint != ready.Fingerprint || v.Port != ready.Port {
		t.Fatal("stop/start changed SSH identity")
	}
	stranger, replacement := testSSHKey(t)
	rejects(stranger, "coder", ssh.PublicKeys(stranger))
	rejects(first, "root", ssh.PublicKeys(first))
	rejects(first, "coder", ssh.Password("synthetic-password"))
	sshSession := login(first)
	shell, e := sshSession.NewSession()
	if e != nil {
		t.Fatal(e)
	}
	out, e := shell.CombinedOutput(`bash -lc '
set -eu
test "$(id -u)" = 10001
command -v codex
test ! -r /var/lib/cloudex-ssh/ssh_host_ed25519_key
test ! -w /run/cloudex-ssh/authorized_keys
printf kept >/workspace/project/ssh-fixture
printf kept >~/.codex/ssh-fixture
test ! -e ~/.codex/AGENTS.md
test ! -e ~/.codex/auth.json
printf "%s\n" "cli_auth_credentials_store = \"file\"" | cmp - ~/.codex/config.toml
'`)
	if e != nil || !strings.Contains(string(out), "/usr/local/bin/codex") {
		t.Fatalf("workspace login shell: %s %v", out, e)
	}
	terminal, e := sshSession.NewSession()
	if e != nil {
		t.Fatal(e)
	}
	if e = terminal.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); e != nil {
		if w, err := db.Workspace(ctx, created.Workspace); err == nil {
			if info, err := c.Inspect(ctx, w); err == nil {
				logs, _, _ := control.ReadOutput(func(out io.Writer) error { return d.Logs(ctx, info.ID, out) })
				t.Logf("OpenSSH daemon: %s", logs)
			}
		}
		t.Fatal("SSH PTY:", e)
	}
	if e = terminal.Run("test -t 0"); e != nil {
		t.Fatal("expected an interactive SSH terminal:", e)
	}
	conn, e := sshSession.Dial("tcp", "127.0.0.1:2222")
	if e != nil {
		t.Fatal("SSH local forwarding:", e)
	}
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	banner, e := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if e != nil || !strings.HasPrefix(banner, "SSH-2.0-") {
		t.Fatalf("forwarded connection: %q %v", banner, e)
	}
	appServerHandshake(t, sshSession)

	var build acceptance
	a.call("POST", path+"/rebuild", nil, 202, &build)
	a.opDone(build.Op)
	if v := get("ready"); v.Port != ready.Port || v.Fingerprint != ready.Fingerprint || v.Alias != ready.Alias {
		t.Fatal("rebuild changed SSH identity")
	}
	if session, e := sshSession.NewSession(); e == nil {
		session.Close()
		t.Fatal("previous Desktop connection survived rebuild")
	}
	sshSession.Close()

	rotate(replacement)
	rejects(first, "coder", ssh.PublicKeys(first))
	if socket, e := jump.Dial("tcp", backend); e == nil {
		socket.Close()
		t.Fatal("already-authenticated gateway connection bypassed key revocation")
	}
	sshSession = login(stranger)
	shell, e = sshSession.NewSession()
	if e != nil {
		t.Fatal(e)
	}
	out, e = shell.CombinedOutput(`set -eu
test "$(cat /workspace/project/ssh-fixture)" = kept
test ! -e ~/.codex/ssh-fixture
grep -Fxq 'Managed SSH instructions' ~/.codex/AGENTS.md
grep -Fxq "model = 'ssh-fixture-model'" ~/.codex/config.toml
test "$(cat ~/.codex/auth.json)" = '{"fixture":"fake-ssh-login"}'
test "$(stat -c '%u:%g:%a' ~/.codex/auth.json)" = 10001:10001:600`)
	if e != nil {
		t.Fatalf("rebuild did not preserve project files and reseed Codex files: %s %v", out, e)
	}
	sshSession.Close()
	rotate()
	get("disabled")
	rejects(stranger, "coder", ssh.PublicKeys(stranger))
	rotate(replacement)
	if v := get("ready"); v.Fingerprint != ready.Fingerprint || v.Port != ready.Port {
		t.Fatal("disable/re-enable changed SSH identity")
	}
	a.call("DELETE", path, map[string]any{"confirm": created.Workspace, "purge": true}, 202, &op)
	a.opDone(op.Op)
	a.call("GET", path+"/access", nil, 404, nil)
	rejects(stranger, "coder", ssh.PublicKeys(stranger))
}

func appServerHandshake(t *testing.T, client *ssh.Client) {
	t.Helper()
	session, e := client.NewSession()
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	stdin, e := session.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdout, e := session.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	session.Stderr = io.Discard
	if e = session.Start(`bash -lc 'cd /workspace/project && exec codex app-server'`); e != nil {
		t.Fatal(e)
	}
	if _, e = fmt.Fprintln(stdin, `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"cloudex_test","title":"Workspace SSH test","version":"1.0.0"}}}`); e != nil {
		t.Fatal(e)
	}
	reply := make(chan error, 1)
	go func() {
		var response struct {
			ID     int
			Result json.RawMessage
			Error  json.RawMessage
		}
		decoder := json.NewDecoder(stdout)
		for {
			if e := decoder.Decode(&response); e != nil {
				reply <- e
				return
			}
			if response.ID != 1 {
				continue
			}
			if len(response.Result) == 0 || len(response.Error) > 0 {
				reply <- fmt.Errorf("app-server initialize failed: %s", response.Error)
				return
			}
			reply <- nil
			return
		}
	}()
	select {
	case e := <-reply:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Codex app-server did not initialize over SSH")
	}
	stdin.Close()
}
