package control

import (
	"context"
	"crypto/ed25519"
	"net/netip"
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
	ci.SSHRevision = remote.Revision(sshTestNetwork, []string{sshTestKey})
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

func TestSSHRouteRequiresAuthorizedKeyAndCurrentContainer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		container func(*runtime.Container)
		workspace func(*testing.T, *store.Store, string)
		routed    bool
	}{
		{name: "running current generation", routed: true},
		{name: "key authorized elsewhere", key: "another-key"},
		{name: "container paused", container: func(c *runtime.Container) { c.State = "paused" }},
		{name: "authorized keys changed after the container was created", container: func(c *runtime.Container) { c.SSHRevision = "outdated" }},
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
