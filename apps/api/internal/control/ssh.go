package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/ssh"
)

type Access struct {
	HasPublicKeys      bool   `json:"has_public_keys"`
	Status             string `json:"status"`
	Message            string `json:"message,omitempty"`
	Host               string `json:"host"`
	Bind               string `json:"bind"`
	Port               int    `json:"port,omitempty"`
	Alias              string `json:"alias"`
	User               string `json:"user"`
	ProjectPath        string `json:"project_path"`
	Fingerprint        string `json:"fingerprint,omitempty"`
	GatewayFingerprint string `json:"gateway_fingerprint,omitempty"`
	KnownHosts         string `json:"known_hosts,omitempty"`
	SSHConfig          string `json:"ssh_config,omitempty"`
}

func (c *Control) Access(ctx context.Context, id string) (Access, error) {
	w, busy, e := c.Store.WorkspaceActivity(ctx, id)
	if e != nil {
		return Access{}, e
	}
	settings, e := c.Store.Settings(ctx)
	if e != nil {
		return Access{}, e
	}
	bind, host := c.SSH.Bind.String(), settings.SSHHost
	if host == "" {
		host = bind
	}
	keys, e := c.Store.SSHKeys(ctx)
	if e != nil {
		return Access{}, e
	}
	v := Access{HasPublicKeys: len(keys) > 0, Host: host, Bind: bind, Port: c.SSH.Port,
		Alias: remote.Alias(w.ID), User: remote.User, ProjectPath: runtime.WorkDir}
	switch {
	case busy:
		v.Status, v.Message = "applying", "Wait for the workspace operation to finish. A previous SSH session may still be active until replacement."
	case w.Observed == "error":
		v.Status, v.Message = "unavailable", w.Error
	default:
		info, e := c.Inspect(ctx, w)
		if e != nil {
			v.Status, v.Message = "unavailable", "The workspace container could not be verified."
			break
		}
		if info.State == "paused" {
			v.Status, v.Message = "paused", store.ErrWorkspacePaused.Error()
			break
		}
		if w.Observed != "running" {
			v.Status, v.Message = "stopped", "Start the workspace before connecting."
			break
		}
		if _, ok := c.gatewayRoute(info); info.State != "running" || !ok {
			v.Status, v.Message = "unavailable", "The running workspace route could not be verified."
			break
		}
		if c.GatewayKey == nil {
			v.Status, v.Message = "unavailable", "The SSH Gateway is not initialized."
			break
		}
		public, e := c.EngineFor(w.HostID).SSHHostKey(ctx, info.ID)
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(public))
		if e != nil || parseErr != nil {
			v.Status, v.Message = "unavailable", "The workspace SSH daemon is not responding."
			break
		}
		v.Status = "ready"
		v.Fingerprint = ssh.FingerprintSHA256(key)
		v.GatewayFingerprint = ssh.FingerprintSHA256(c.GatewayKey)
		v.KnownHosts = c.GatewayKeyAlias + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(c.GatewayKey))) + "\n" + v.Alias + " " + public
		jump := v.Alias + "-gateway"
		v.SSHConfig = fmt.Sprintf("Host %s\n    HostName %s\n    Port %d\n    User %s\n    HostKeyAlias %s\n    StrictHostKeyChecking yes\n\n"+
			"Host %s\n    HostName %s\n    Port %d\n    User %s\n    ProxyJump %s\n    HostKeyAlias %s\n    StrictHostKeyChecking yes\n",
			jump, v.Host, v.Port, v.Alias, c.GatewayKeyAlias, v.Alias, v.Alias, remote.BackendPort, v.User, jump, v.Alias)
	}
	return v, nil
}

func (c *Control) PrepareGateway(ctx context.Context, instance string) (ssh.Signer, error) {
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	nonce := make([]byte, c.Key.NonceSize())
	rand.Read(nonce)
	aad := []byte("cloudex:ssh_gateway:v1")
	sealed, e := c.Store.GatewayIdentity(ctx, c.Key.Seal(nonce, nonce, seed, aad))
	if e != nil {
		return nil, e
	}
	if len(sealed) < len(nonce) {
		return nil, errors.New("invalid SSH Gateway host key")
	}
	plain, e := c.Key.Open(nil, sealed[:len(nonce)], sealed[len(nonce):], aad)
	if e != nil || len(plain) != ed25519.SeedSize {
		return nil, errors.New("cannot decrypt SSH Gateway host key with CLOUDEX_MASTER_KEY")
	}
	signer, e := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(plain))
	if e != nil {
		return nil, e
	}
	c.GatewayKey = signer.PublicKey()
	c.GatewayKeyAlias = "cloudex-gateway-" + instance
	return signer, nil
}

func (c *Control) AuthorizeSSH(ctx context.Context, id, public string) error {
	_, e := c.Store.Workspace(ctx, id)
	if e != nil {
		return errors.New("workspace access denied")
	}
	allowed, e := c.Store.HasSSHKey(ctx, strings.TrimSpace(public))
	if e != nil || !allowed {
		return errors.New("workspace access denied")
	}
	return nil
}

func (c *Control) gatewayRoute(info runtime.Container) (netip.AddrPort, bool) {
	ip := info.NetworkIPs[c.SSH.Network]
	if !ip.Is4() {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip, remote.BackendPort), true
}

func (c *Control) AdmitSSH(ctx context.Context, id, public string, target remote.Target, accept func() error) error {
	current, err := c.ResolveSSH(ctx, id, public)
	if err != nil || current != target {
		return errors.New("workspace route changed; reconnect")
	}
	return c.Store.AdmitSSH(ctx, id, strings.TrimSpace(public), target.Generation, accept)
}

func (c *Control) ResolveSSH(ctx context.Context, id, public string) (remote.Target, error) {
	allowed, e := c.Store.HasSSHKey(ctx, strings.TrimSpace(public))
	if e != nil || !allowed {
		return remote.Target{}, errors.New("workspace access denied")
	}
	w, _, e := c.Store.ReadyContainer(ctx, id)
	if e != nil {
		return remote.Target{}, errors.New("workspace is stopped or changing; retry when running")
	}
	info, e := c.Inspect(ctx, w)
	if e != nil || info.State != "running" {
		return remote.Target{}, errors.New("workspace generation is unavailable")
	}
	address, ok := c.gatewayRoute(info)
	if !ok {
		return remote.Target{}, errors.New("the workspace is not attached to the SSH Gateway network")
	}
	target := remote.Target{Address: address, Generation: w.Generation}
	if c.Agents != nil {
		target.HostID, target.ContainerID = w.HostID, info.ID
	}
	return target, nil
}

func (c *Control) DialSSH(ctx context.Context, target remote.Target) (net.Conn, error) {
	if c.Agents != nil {
		return c.Agents.DialSSH(ctx, target.HostID, target.ContainerID, target.Address)
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", target.Address.String())
}

func (c *Control) waitForSSH(ctx context.Context, engine runtime.Engine, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	timer := time.NewTicker(250 * time.Millisecond)
	defer timer.Stop()
	for {
		// A probe cut off by this wait's own deadline is a daemon that did not
		// start, not a lost Host.
		if _, e := engine.SSHHostKey(ctx, id); e == nil || (errors.Is(e, runtime.ErrHostUnavailable) && ctx.Err() == nil) {
			return e
		}
		select {
		case <-ctx.Done():
			return errors.New("workspace SSH daemon did not start; check container logs and rebuild the supported workspace image")
		case <-timer.C:
		}
	}
}

func (c *Control) syncContainerSSHKeys(ctx context.Context, engine runtime.Engine, workspace, id string, afterSync func() error) error {
	unlock, err := c.Store.LockSSHKeySync(ctx, workspace)
	if err != nil {
		return err
	}
	defer unlock()
	keys, err := c.Store.SSHKeys(ctx)
	if err != nil {
		return err
	}
	if err := engine.SyncAuthorizedKeys(ctx, id, keys); err != nil {
		return err
	}
	if afterSync != nil {
		return afterSync()
	}
	return nil
}

func (c *Control) SyncSSHKeys(ctx context.Context) error {
	hosts, err := c.Store.Hosts(ctx)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	failures := make(chan error, len(hosts))
	for _, host := range hosts {
		if c.Agents != nil && host.Status != "online" {
			continue // The Host gets the current keys when it reconnects.
		}
		workers.Go(func() {
			err := c.syncHostSSHKeys(ctx, host.ID, c.EngineFor(host.ID))
			if err != nil {
				failures <- err
			}
		})
	}
	workers.Wait()
	close(failures)
	var result error
	for err := range failures {
		result = errors.Join(result, err)
	}
	return result
}

func (c *Control) SyncConnectedHostSSHKeys(ctx context.Context, host string, engine runtime.Engine) {
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	logError(ctx, "sync SSH public keys on reconnected Host", c.syncHostSSHKeys(check, host, engine))
}

func (c *Control) syncHostSSHKeys(ctx context.Context, host string, engine runtime.Engine) error {
	workspaces, err := c.Store.HostWorkspaces(ctx, host)
	if err != nil {
		return err
	}
	var result error
	for _, w := range workspaces {
		if err := c.syncWorkspaceSSHKeys(ctx, w, engine); err != nil {
			result = errors.Join(result, fmt.Errorf("sync SSH keys for %s: %w", w.ID, err))
		}
	}
	return result
}

func (c *Control) syncWorkspaceSSHKeys(ctx context.Context, w store.Workspace, engine runtime.Engine) error {
	// Inspect under the same lock as Start so a new container cannot start
	// with an older key list after this update skips a stopped generation.
	unlock, err := c.Store.LockSSHKeySync(ctx, w.ID)
	if err != nil {
		return err
	}
	defer unlock()
	generations, err := c.Store.SSHSyncGenerations(ctx, w.ID)
	if err != nil {
		return err
	}
	keys, err := c.Store.SSHKeys(ctx)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		current, err := engine.Inspect(ctx, spec(w, generation))
		if errors.Is(err, runtime.ErrMissing) {
			continue
		}
		if err != nil {
			return err
		}
		if current.State == "running" {
			if err := engine.SyncAuthorizedKeys(ctx, current.ID, keys); err != nil && !errors.Is(err, runtime.ErrMissing) {
				return err
			}
		}
	}
	return nil
}
