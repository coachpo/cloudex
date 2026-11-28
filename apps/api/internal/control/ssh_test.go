package control

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/ssh"
)

const sshTestNetwork, sshTestKey = "fixture-network", "fixture-key"

func seedSSHWorkspace(t *testing.T) (*Control, store.Workspace, *runtime.Container) {
	t.Helper()
	db, w, ci := seedRecoveryWorkspace(t, "running", []string{sshTestKey})
	ci.NetworkIPs = map[string]netip.Addr{sshTestNetwork: netip.MustParseAddr("172.19.0.2")}
	c := &Control{Store: db, SSH: remote.Config{Network: sshTestNetwork}, Engine: recoveryEngine{
		inspect: func(context.Context) (runtime.Container, error) { return ci, nil },
	}}
	return c, w, &ci
}

func TestSSHClientConfigUsesCurrentHostSetting(t *testing.T) {
	ctx := t.Context()
	c, w, ci := seedSSHWorkspace(t)
	c.SSH.Bind, c.SSH.Port = netip.MustParseAddr("127.0.0.1"), 2222
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	c.GatewayKey, c.GatewayKeyAlias = signer.PublicKey(), "fixture-gateway"
	public := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	c.Engine = recoveryEngine{
		inspect: func(context.Context) (runtime.Container, error) { return *ci, nil },
		hostKey: func() (string, error) { return public, nil },
	}
	assertHost := func(want string) Access {
		t.Helper()
		got, err := c.Access(ctx, w.ID)
		if err != nil || got.Status != "ready" || got.Host != want ||
			!strings.Contains(got.SSHConfig, "\n    HostName "+want+"\n") {
			t.Fatalf("access setting %q: %+v, %v", want, got, err)
		}
		return got
	}
	before := assertHost("127.0.0.1")
	if err := c.Store.SetAccessSettings(ctx, "gateway.example.org", ""); err != nil {
		t.Fatal(err)
	}
	after := assertHost("gateway.example.org")
	if after.KnownHosts != before.KnownHosts || !strings.Contains(after.SSHConfig, "\n    HostKeyAlias fixture-gateway\n") {
		t.Fatalf("changing the SSH hostname changed the known_hosts identity: %+v", after)
	}
	if err := c.Store.SetAccessSettings(ctx, "", ""); err != nil {
		t.Fatal(err)
	}
	assertHost("127.0.0.1")
}

func TestSSHClientConfigSeparatesHostAliasFromWorkspaceIdentity(t *testing.T) {
	const id = "9659fa2a889fc2bbad8ef990a2b45e3a"
	const identity = "cloudex-" + id
	for _, tc := range []struct {
		name  string
		alias string
	}{
		{name: "http-gateway-check", alias: "cloudex-http-gateway-check"},
		{name: "HTTP_Gateway.Check", alias: "cloudex-http_gateway.check"},
		{name: "A", alias: "cloudex-a"},
		{name: "gateway check", alias: "cloudex-9659fa2a"},
		{name: "工作区", alias: "cloudex-9659fa2a"},
		{name: "Kelvin", alias: "cloudex-9659fa2a"},
		{name: "gateway.", alias: "cloudex-9659fa2a"},
		{name: "gateway_", alias: "cloudex-9659fa2a"},
		{name: "gateway-", alias: "cloudex-9659fa2a"},
		// "api" has the gateway Host "cloudex-api-gateway"; a workspace must not reuse it.
		{name: "api-gateway", alias: "cloudex-9659fa2a"},
		{name: "API-Gateway", alias: "cloudex-9659fa2a"},
		{name: "api-gateway-v2", alias: "cloudex-api-gateway-v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			c, _, ci := seedSSHWorkspace(t)
			ci.ID = "alias-fixture-container"
			c.SSH.Bind, c.SSH.Port = netip.MustParseAddr("127.0.0.1"), 2222
			signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
			if err != nil {
				t.Fatal(err)
			}
			c.GatewayKey, c.GatewayKeyAlias = signer.PublicKey(), "fixture-gateway"
			public := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
			c.Engine = recoveryEngine{
				inspect: func(context.Context) (runtime.Container, error) { return *ci, nil },
				hostKey: func() (string, error) { return public, nil },
			}
			w := store.Workspace{ID: id, Name: tc.name, Image: ci.ImageID, Branch: "main", CPU: 500, Memory: 512, Desired: "running"}
			op, err := c.Store.Create(ctx, w, "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Store.FinishOp(ctx, op, "running", &store.Container{ID: ci.ID, ImageID: ci.ImageID, Generation: op.Target, Status: ci.State}, ""); err != nil {
				t.Fatal(err)
			}
			access, err := c.Access(ctx, id)
			if err != nil || access.Status != "ready" || access.Alias != tc.alias {
				t.Fatalf("access alias for %q: %+v, %v", tc.name, access, err)
			}
			jump := tc.alias + "-gateway"
			want := fmt.Sprintf(`Host %s
    HostName 127.0.0.1
    Port 2222
    User %s
    HostKeyAlias fixture-gateway
    StrictHostKeyChecking yes

Host %s
    HostName %s
    Port 2222
    User coder
    ProxyJump %s
    HostKeyAlias %s
    StrictHostKeyChecking yes
`, jump, identity, tc.alias, identity, jump, identity)
			if access.SSHConfig != want {
				t.Errorf("SSH config =\n%s\nwant:\n%s", access.SSHConfig, want)
			}
			if want := "fixture-gateway " + public + identity + " " + public; access.KnownHosts != want {
				t.Errorf("known_hosts = %q, want %q", access.KnownHosts, want)
			}
		})
	}
}

func TestSSHRouteRequiresAuthorizedKeyAndCurrentContainer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		container func(*runtime.Container)
		workspace func(*testing.T, *store.Store, string)
		routed    bool
	}{
		{name: "running current generation", routed: true},
		{name: "unknown key", key: "another-key"},
		{name: "container paused", container: func(c *runtime.Container) { c.State = "paused" }},
		{name: "not attached to the gateway network", container: func(c *runtime.Container) { c.NetworkIPs = nil }},
		{name: "recorded container was replaced", container: func(c *runtime.Container) { c.ID = "another-container" }},
		{name: "lifecycle operation queued", workspace: func(t *testing.T, db *store.Store, id string) {
			if _, err := db.Enqueue(t.Context(), id, "stop", false, "", ""); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "observed as stopped", workspace: func(t *testing.T, db *store.Store, id string) {
			current, err := db.Workspace(t.Context(), id)
			if err == nil {
				err = db.Observe(t.Context(), current, "stopped", "", false, false)
			}
			if err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			c, w, ci := seedSSHWorkspace(t)
			want := remote.Target{Address: netip.AddrPortFrom(ci.NetworkIPs[sshTestNetwork], remote.BackendPort), Generation: w.Generation}
			if tc.container != nil {
				tc.container(ci)
			}
			if tc.workspace != nil {
				tc.workspace(t, c.Store, w.ID)
			}
			key := sshTestKey
			if tc.key != "" {
				key = tc.key
			}
			if err := c.AuthorizeSSH(ctx, w.ID, key); (err == nil) != (key == sshTestKey) {
				t.Fatalf("gateway login for key %q: %v", key, err)
			}
			target, err := c.ResolveSSH(ctx, w.ID, key)
			if tc.routed {
				if err != nil || target != want {
					t.Fatalf("route = %+v, want %+v: %v", target, want, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("gateway routed to %+v", target)
			}
		})
	}
}

func TestSSHAdmissionRejectsRouteThatChangedAfterDial(t *testing.T) {
	ctx := t.Context()
	c, w, ci := seedSSHWorkspace(t)
	dialed, err := c.ResolveSSH(ctx, w.ID, sshTestKey)
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	accept := func() error { accepted++; return nil }
	if err := c.AdmitSSH(ctx, w.ID, sshTestKey, dialed, accept); err != nil || accepted != 1 {
		t.Fatalf("unchanged route was not admitted: accepted=%d err=%v", accepted, err)
	}
	// The container came back at another address after the tunnel was dialed.
	ci.NetworkIPs = map[string]netip.Addr{sshTestNetwork: netip.MustParseAddr("172.19.0.3")}
	if err := c.AdmitSSH(ctx, w.ID, sshTestKey, dialed, accept); err == nil || accepted != 1 {
		t.Fatalf("stale route was admitted: accepted=%d err=%v", accepted, err)
	}
}

type sshSyncRuntime struct {
	runtime.Engine
	inspect func(runtime.Spec) (runtime.Container, error)
	sync    func(string, []string) error
}

func (e sshSyncRuntime) Inspect(_ context.Context, spec runtime.Spec) (runtime.Container, error) {
	return e.inspect(spec)
}

func (e sshSyncRuntime) SyncAuthorizedKeys(_ context.Context, id string, keys []string) error {
	return e.sync(id, keys)
}

func TestSyncSSHKeysFindsRunningCreateBeforeOutcomeIsRecorded(t *testing.T) {
	ctx := t.Context()
	db, _, _ := seedRecoveryWorkspace(t, "stopped", nil)
	w := store.Workspace{ID: store.ID(), Name: "pending", Image: "fixture", Branch: "main", CPU: 500, Memory: 512, Desired: "running"}
	op, err := db.Create(ctx, w, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimOp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSSHKeys(ctx, []string{sshTestKey}); err != nil {
		t.Fatal(err)
	}
	var writes int
	engine := sshSyncRuntime{
		inspect: func(spec runtime.Spec) (runtime.Container, error) {
			if spec.WorkspaceID == w.ID && spec.Generation == op.Target {
				return runtime.Container{ID: "running-candidate", State: "running"}, nil
			}
			return runtime.Container{ID: "stopped-container", State: "exited"}, nil
		},
		sync: func(id string, keys []string) error {
			if id != "running-candidate" || !slices.Equal(keys, []string{sshTestKey}) {
				return fmt.Errorf("synced keys %v to %s", keys, id)
			}
			writes++
			return nil
		},
	}
	c := &Control{Store: db, Engine: engine}
	if err := c.SyncSSHKeys(ctx); err != nil || writes != 1 {
		t.Fatalf("key copies = %d, error = %v", writes, err)
	}
}
