package runtime

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"golang.org/x/crypto/ssh"
)

func (d *Docker) DialWorkspaceSSH(ctx context.Context, id string, address netip.AddrPort, network string) (_ net.Conn, outErr error) {
	defer d.classifyOutcome(&outErr)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := d.ownedContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.State == nil || c.State.Status != "running" ||
		c.HostConfig == nil || len(c.HostConfig.PortBindings[sshContainerPort]) > 0 || c.NetworkSettings == nil ||
		len(c.NetworkSettings.Ports[sshContainerPort]) > 0 || !address.Addr().Is4() || address.Port() != remote.BackendPort {
		return nil, ErrOwnership
	}
	endpoint := c.NetworkSettings.Networks[network]
	if endpoint == nil || endpoint.IPAddress != address.Addr() {
		return nil, ErrOwnership
	}
	return d.dialWorkspace(ctx, "tcp", address.String())
}

func (d *Docker) SSHHostKey(ctx context.Context, id string) (string, error) {
	var out bytes.Buffer
	// Probe the daemon inside the owned container, not an arbitrary address
	// or an untrusted host supplied by a client.
	code, e := d.Run(ctx, id, Exec{Args: []string{"ssh-keyscan", "-T", "1", "-t", "ed25519", "-p", "2222", "127.0.0.1"}, Dir: "/workspace", Stdout: &out})
	if e != nil {
		return "", e
	}
	if code != 0 {
		return "", errors.New("workspace SSH daemon did not respond")
	}
	_, _, key, _, _, e := ssh.ParseKnownHosts(out.Bytes())
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}

func (d *Docker) SyncAuthorizedKeys(ctx context.Context, id string, keys []string) error {
	if _, err := d.ownedContainer(ctx, id); err != nil {
		return err
	}
	content := ""
	if len(keys) > 0 {
		content = strings.Join(keys, "\n") + "\n"
	}
	return d.copyWorkspaceFiles(ctx, id, "/run/cloudex-ssh", []workspaceFile{
		{name: "authorized_keys", content: content, mode: 0644},
	}, 0)
}
