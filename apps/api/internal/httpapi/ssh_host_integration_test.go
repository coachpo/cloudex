package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentlink"
	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/hostmanager"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

func TestSSHHostWorkspaceAccess(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_SSH_HOST") != "1" {
		t.Skip("set CLOUDEX_TEST_SSH_HOST=1 with an isolated SSH Docker Host")
	}
	db := newDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	instance, err := db.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	conf, err := remote.NewConfig("127.0.0.1", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), os.Getenv("CLOUDEX_TEST_SSH_NETWORK"))
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &control.Control{Store: db, Key: key, SSH: conf}
	manager, err := hostmanager.New(ctx, db, instance, conf.Network, key, agentlink.NewHub(nil, nil, nil, nil), ctrl.SyncConnectedHostSSHKeys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	ctrl.Agents = manager
	api := newTestServer(t, &Server{Control: ctrl, SSHHosts: manager, Origin: testOrigin})
	api.call("POST", "/auth/setup", map[string]string{"username": "tester", "password": syntheticPassword}, 200, nil)
	gatewayKey, err := ctrl.PrepareGateway(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	worker, gateway := make(chan error, 1), make(chan error, 1)
	go func() { worker <- ctrl.Run(ctx) }()
	go func() { gateway <- remote.Serve(ctx, listener, gatewayKey, ctrl) }()
	t.Cleanup(func() {
		cancel()
		if err := <-worker; err != nil {
			t.Error(err)
		}
		if err := <-gateway; err != nil {
			t.Error(err)
		}
	})

	address := os.Getenv("CLOUDEX_TEST_HOST_ADDRESS")
	var probe struct {
		HostKey     string `json:"host_key"`
		Fingerprint string `json:"fingerprint"`
	}
	api.call("POST", "/hosts/ssh/probe", map[string]string{"address": address}, 200, &probe)
	expectedKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(os.Getenv("CLOUDEX_TEST_HOST_PUBLIC_KEY")))
	if err != nil {
		t.Fatal("could not parse the independently supplied SSH host public key")
	}
	if probe.Fingerprint != ssh.FingerprintSHA256(expectedKey) || probe.HostKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(expectedKey))) {
		t.Fatalf("unexpected SSH server identity %s", probe.Fingerprint)
	}
	credentials := map[string]string{"address": address, "username": os.Getenv("CLOUDEX_TEST_HOST_USERNAME"), "host_key": probe.HostKey}
	for field, variable := range map[string]string{"password": "CLOUDEX_TEST_HOST_PASSWORD", "private_key": "CLOUDEX_TEST_HOST_PRIVATE_KEY", "passphrase": "CLOUDEX_TEST_HOST_PASSPHRASE"} {
		if v := os.Getenv(variable); v != "" {
			credentials[field] = v
		}
	}
	credentials["name"] = "SSH acceptance"
	var host store.Host
	api.call("POST", "/hosts/ssh", credentials, 201, &host)
	if host.ID == "" || host.ConnectionType != "ssh" {
		t.Fatal("Add host did not return an SSH Host")
	}
	hostPath := "/hosts/" + host.ID
	waitOnline := func() {
		t.Helper()
		waitFor(t, "SSH Host ready for placement", func() bool {
			var report store.Host
			api.call("GET", hostPath, nil, 200, &report)
			if report.Status != "online" || report.Engine == "" {
				return false
			}
			for _, image := range report.Images {
				if image.Image == store.DefaultWorkspaceImage && image.Ready {
					return true
				}
			}
			return false
		})
	}
	waitOnline()
	delete(credentials, "name")
	api.call("PUT", hostPath+"/ssh", credentials, 200, &host)
	waitOnline()
	t.Log("Host key confirmed; added SSH Host, updated credentials and received real Docker inventory")
	admin, pub := testSSHKey(t)
	api.call("PUT", "/settings/ssh-keys", map[string]any{"public_keys": []string{pub}}, 200, nil)
	res := api.raw("POST", "/workspaces", testOrigin, "ssh-host-create", map[string]any{
		"name": "SSH Host acceptance", "placement": map[string]string{"type": "manual", "host_id": host.ID},
		"resources": map[string]int{"cpu_milli": 500, "memory_mb": 512},
	})
	defer res.Body.Close()
	if res.StatusCode != 202 {
		data, _ := io.ReadAll(res.Body)
		t.Fatalf("create workspace: %d: %s", res.StatusCode, data)
	}
	var created struct{ Data acceptance }
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	id := created.Data.Workspace
	path := "/workspaces/" + id
	deleted := false
	t.Cleanup(func() {
		// The test owns this workspace and any host mode it changed.
		if deleted {
			return
		}
		api.call("PUT", hostPath+"/mode", map[string]string{"mode": "enabled"}, 200, nil)
		waitOnline()
		var deletion acceptance
		api.call("DELETE", path, map[string]any{"confirm": id, "purge": true}, 202, &deletion)
		api.opDone(deletion.Op)
	})
	api.opDone(created.Data.Op)
	out := api.shell(path, 113, 37, "printf %s "+instance+" > ssh-host-test; cp ssh-host-test ~/ssh-host-test; stty size; printf 'PTY%s\\n' '_READY'", "PTY_READY")
	if !strings.Contains(out, "37 113") {
		t.Fatalf("remote Web Terminal resize missing: %q", out)
	}
	access := func() control.Access {
		t.Helper()
		var view control.Access
		api.call("GET", path+"/access", nil, 200, &view)
		if view.Status != "ready" {
			t.Fatalf("workspace SSH is %s: %s", view.Status, view.Message)
		}
		return view
	}
	initial := access()
	endpoint := connectGatewayWorkspace(t, id, initial, listener.Addr().String(), admin)
	shell, err := endpoint.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := shell.Output("cat /workspace/project/ssh-host-test")
	shell.Close()
	if err != nil || string(readBack) != instance {
		t.Fatalf("Gateway did not read Web Terminal file: %q, %v", readBack, err)
	}
	t.Log("Web Terminal resized and Gateway read the same workspace file")
	terminal := api.openTerminal(ctx, path)
	defer terminal.CloseNow()
	// Retry after an unhealthy inventory observation with a live Gateway and PTY.
	if err := db.SSHDegraded(ctx, host.ID, host.SSHRevision, "temporary inventory failure"); err != nil {
		t.Fatal(err)
	}
	api.call("POST", hostPath+"/ssh/reconnect", nil, 200, nil)
	waitOnline()
	t.Run("CodexAppServer", func(t *testing.T) { appServerHandshake(t, endpoint) })
	readyCtx, stopReady := context.WithTimeout(ctx, 8*time.Second)
	defer stopReady()
	if err := terminal.Write(readyCtx, websocket.MessageBinary, []byte("printf 'CHECK%s\\n' '_READY'\r")); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "CHECK_READY") {
		_, b, err := terminal.Read(readyCtx)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(b)
	}
	stopReady()
	lost := make(chan error, 1)
	go func() { lost <- endpoint.Wait() }()
	api.call("PUT", hostPath+"/mode", map[string]string{"mode": "disabled"}, 200, nil)
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("disabling Host left its active SSH tunnel open")
	}
	closedCtx, stopClosed := context.WithTimeout(ctx, 5*time.Second)
	defer stopClosed()
	for {
		_, _, err := terminal.Read(closedCtx)
		if closedCtx.Err() != nil {
			t.Fatal("disabling Host left its Web Terminal open")
		}
		if err != nil {
			break
		}
	}
	stopClosed()
	api.call("PUT", hostPath+"/mode", map[string]string{"mode": "enabled"}, 200, nil)
	waitOnline()
	lifecycle := func(op string, body any) {
		t.Helper()
		var receipt acceptance
		api.call("POST", path+"/"+op, body, 202, &receipt)
		api.opDone(receipt.Op)
	}
	inspect := func() runtime.Container {
		t.Helper()
		var container runtime.Container
		api.call("GET", path+"/inspect", nil, 200, &container)
		return container
	}
	before := inspect()
	lifecycle("stop", nil)
	if inspect().State != "exited" {
		t.Fatal("Stop did not stop the remote container")
	}
	lifecycle("start", nil)
	if after := inspect(); after.ID != before.ID || after.State != "running" {
		t.Fatal("Start did not resume the same remote container")
	}
	lifecycle("rebuild", nil)
	if inspect().ID == before.ID {
		t.Fatal("Rebuild reused old container")
	}
	api.shell(path, 80, 24, "cmp ssh-host-test ~/ssh-host-test && printf 'REBUILD%s\\n' '_OK'", "REBUILD_OK")
	lifecycle("reset", map[string]string{"confirm": id})
	api.shell(path, 80, 24, "test ! -e ssh-host-test && test \"$(cat ~/ssh-host-test)\" = "+instance+" && printf 'RESET%s\\n' '_OK'", "RESET_OK")
	next := access()
	if next.Fingerprint != initial.Fingerprint {
		t.Fatal("Reset replaced the Workspace SSH host key")
	}
	resumed := connectGatewayWorkspace(t, id, next, listener.Addr().String(), admin)
	resumed.Close()
	var deletion acceptance
	api.call("DELETE", path, map[string]any{"confirm": id, "purge": true}, 202, &deletion)
	api.opDone(deletion.Op)
	deleted = true
	api.call("GET", path, nil, 404, nil)
	t.Log("Disable closed active tunnel; resumed Host completed Stop, Start, Rebuild, Reset and Delete")
}

func connectGatewayWorkspace(t *testing.T, id string, view control.Access, address string, admin ssh.Signer) *ssh.Client {
	t.Helper()
	_, _, outer, _, rest, err := ssh.ParseKnownHosts([]byte(view.KnownHosts))
	if err != nil {
		t.Fatal(err)
	}
	_, _, inner, _, _, err := ssh.ParseKnownHosts(rest)
	if err != nil {
		t.Fatal(err)
	}
	identity := remote.WorkspaceIdentity(id)
	jump, err := sshClient(address, identity, outer, ssh.PublicKeys(admin))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jump.Close() })
	target := net.JoinHostPort(identity, strconv.Itoa(remote.BackendPort))
	channel, err := jump.Dial("tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := wrapSSH(channel, target, view.User, inner, ssh.PublicKeys(admin))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
