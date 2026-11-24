package remote

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Target struct {
	Address     netip.AddrPort
	Generation  int
	HostID      string
	ContainerID string
}

type Resolver interface {
	AuthorizeSSH(ctx context.Context, id, publicKey string) error
	ResolveSSH(ctx context.Context, id, publicKey string) (Target, error)
	AdmitSSH(ctx context.Context, id, publicKey string, target Target, accept func() error) error
}

// Serve forwards only OpenSSH ProxyJump channels. The nested SSH connection
// authenticates directly to the workspace and keeps its own host-key trust.
func Serve(ctx context.Context, listener net.Listener, signer ssh.Signer, resolver Resolver) error {
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	var clients sync.WaitGroup
	defer func() { cancel(); clients.Wait() }()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			id, ok := WorkspaceID(conn.User())
			if !ok {
				return nil, errors.New("invalid workspace login")
			}
			public := string(ssh.MarshalAuthorizedKey(key))
			auth, end := context.WithTimeout(ctx, 3*time.Second)
			defer end()
			if resolver.AuthorizeSSH(auth, id, public) != nil {
				return nil, errors.New("workspace access denied")
			}
			return &ssh.Permissions{Extensions: map[string]string{"key": public, "fingerprint": ssh.FingerprintSHA256(key)}}, nil
		},
	}
	config.AddHostKey(signer)
	for {
		conn, e := listener.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		clients.Go(func() { serveClient(ctx, conn, config, resolver) })
	}
}

func serveClient(ctx context.Context, conn net.Conn, config *ssh.ServerConfig, resolver Resolver) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	client, channels, requests, e := ssh.NewServerConn(conn, config)
	if e != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer client.Close()
	ctx, cancel := context.WithCancel(ctx)
	var tunnels sync.WaitGroup
	defer func() { cancel(); tunnels.Wait() }()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		if channel.ChannelType() != "direct-tcpip" {
			_ = channel.Reject(ssh.UnknownChannelType, "use ProxyJump to connect to a workspace")
			continue
		}
		var dest struct {
			Host       string
			Port       uint32
			Origin     string
			OriginPort uint32
		}
		if ssh.Unmarshal(channel.ExtraData(), &dest) != nil || dest.Host != client.User() || dest.Port != BackendPort {
			_ = channel.Reject(ssh.Prohibited, "only the authorized workspace SSH endpoint may be forwarded")
			continue
		}
		id, _ := WorkspaceID(client.User())
		tunnels.Go(func() {
			dial, end := context.WithTimeout(ctx, 5*time.Second)
			target, e := resolver.ResolveSSH(dial, id, client.Permissions.Extensions["key"])
			end()
			if e != nil {
				_ = channel.Reject(ssh.Prohibited, e.Error())
				return
			}
			// Dial only the inspected generation, never the client-supplied host.
			var upstream net.Conn
			if agent, ok := resolver.(interface {
				DialSSH(context.Context, Target) (net.Conn, error)
			}); ok {
				upstream, e = agent.DialSSH(ctx, target)
			} else {
				upstream, e = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", target.Address.String())
			}
			if e != nil {
				_ = channel.Reject(ssh.ConnectionFailed, "workspace sshd is not reachable")
				return
			}
			defer upstream.Close()
			var stream ssh.Channel
			var requests <-chan *ssh.Request
			attempted := false
			admit, end := context.WithTimeout(ctx, 5*time.Second)
			defer end()
			e = resolver.AdmitSSH(admit, id, client.Permissions.Extensions["key"], target, func() error {
				attempted = true
				// A stalled peer must not hold this workspace's admission gate open.
				stop := context.AfterFunc(admit, func() { conn.Close() })
				defer stop()
				var err error
				stream, requests, err = channel.Accept()
				return err
			})
			if e != nil {
				if stream != nil {
					_ = stream.Close()
				} else if !attempted {
					_ = channel.Reject(ssh.Prohibited, "workspace authorization or route changed; reconnect")
				}
				return
			}
			end()
			go ssh.DiscardRequests(requests)
			fingerprint := client.Permissions.Extensions["fingerprint"]
			start := time.Now()
			slog.Info("SSH tunnel opened", "host", target.HostID, "workspace", id, "generation", target.Generation, "key_fingerprint", fingerprint)
			defer func() {
				slog.Info("SSH tunnel closed", "host", target.HostID, "workspace", id, "generation", target.Generation, "key_fingerprint", fingerprint, "duration", time.Since(start))
			}()
			relay(ctx, stream, upstream)
		})
	}
}

func relay(ctx context.Context, stream ssh.Channel, upstream net.Conn) {
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { upstream.Close(); stream.Close() })
	defer stop()
	var copyDone sync.WaitGroup
	copyDone.Go(func() {
		if _, e := io.Copy(upstream, stream); e != nil {
			upstream.Close()
			stream.Close()
			return
		}
		if half, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	})
	if _, e := io.Copy(stream, upstream); e != nil {
		upstream.Close()
		stream.Close()
	} else {
		_ = stream.CloseWrite()
	}
	copyDone.Wait()
}
