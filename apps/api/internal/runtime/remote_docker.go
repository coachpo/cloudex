package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Remote operations with a lost response must be retried against the same
// operation and resource identity. Docker's explicit HTTP rejection is final.
func (d *Docker) classifyOutcome(result *error) {
	err := *result
	if !d.remote || err == nil || errors.Is(err, ErrHostUnavailable) {
		return
	}
	var networkErr net.Error
	if client.IsErrConnectionFailed(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errors.As(err, &networkErr) {
		*result = fmt.Errorf("%w: %w", ErrHostUnavailable, err)
	}
}

// The local agent validates ID-only requests at its RPC boundary. Direct SSH
// reaches Docker without that agent and must check the same ownership first.
func (d *Docker) validateRemoteID(ctx context.Context, id string) error {
	if !d.remote {
		return nil
	}
	return d.ValidateContainer(ctx, id)
}

func (d *Docker) EnsureSSHNetwork(ctx context.Context, name string) (outErr error) {
	defer d.classifyOutcome(&outErr)
	if !remote.ValidWorkspaceNetwork(name) {
		return errors.New("a dedicated Docker bridge network is required")
	}
	network, err := d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if errdefs.IsNotFound(err) {
		_, err = d.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge"})
		if err != nil && !errdefs.IsConflict(err) {
			return err
		}
		network, err = d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	}
	if err != nil {
		return err
	}
	n := network.Network
	if n.Name == name && n.Driver == "bridge" && n.Scope == "local" && !n.ConfigOnly && n.Options["com.docker.network.bridge.default_bridge"] != "true" {
		for _, config := range n.IPAM.Config {
			if config.Subnet.Addr().Is4() {
				return nil
			}
		}
	}
	return fmt.Errorf("network %q must be a dedicated IPv4 Docker bridge", name)
}
