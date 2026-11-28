package remote

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// gatewayResolver authorizes one key per workspace and routes it to a local listener.
type gatewayResolver struct {
	mu     sync.Mutex
	keys   map[string]string
	routes map[string]netip.AddrPort
}

func (r *gatewayResolver) allowed(id, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keys[id] != "" && r.keys[id] == strings.TrimSpace(key)
}

func (r *gatewayResolver) revoke(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.keys, id)
}

func (r *gatewayResolver) AuthorizeSSH(_ context.Context, id, key string) error {
	if !r.allowed(id, key) {
		return errors.New("workspace access denied")
	}
	return nil
}

func (r *gatewayResolver) ResolveSSH(_ context.Context, id, key string) (Target, error) {
	if !r.allowed(id, key) {
		return Target{}, errors.New("workspace access denied")
	}
	return Target{Address: r.routes[id], Generation: 1}, nil
}

func (r *gatewayResolver) AdmitSSH(_ context.Context, id, key string, _ Target, accept func() error) error {
	if !r.allowed(id, key) {
		return errors.New("workspace route changed; reconnect")
	}
	return accept()
}

func gatewayKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// workspaceEndpoint stands in for a workspace sshd and identifies itself to each connection.
func workspaceEndpoint(t *testing.T, name string) netip.AddrPort {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, name+"\n")
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return netip.MustParseAddrPort(listener.Addr().String())
}

func TestGatewayForwardsOnlyToTheAuthenticatedWorkspace(t *testing.T) {
	first, second := strings.Repeat("a", 32), strings.Repeat("b", 32)
	host, _ := gatewayKey(t)
	firstKey, firstLine := gatewayKey(t)
	secondKey, secondLine := gatewayKey(t)
	secondEndpoint := workspaceEndpoint(t, "second")
	resolver := &gatewayResolver{
		keys:   map[string]string{first: firstLine, second: secondLine},
		routes: map[string]netip.AddrPort{first: workspaceEndpoint(t, "first"), second: secondEndpoint},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, listener, host, resolver) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	login := func(user string, key ssh.Signer) (*ssh.Client, error) {
		return ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
			HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second})
	}
	reach := func(client *ssh.Client, destination string) (string, error) {
		conn, err := client.Dial("tcp", destination)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		name, err := bufio.NewReader(conn).ReadString('\n')
		return strings.TrimSpace(name), err
	}

	for _, user := range []string{WorkspaceIdentity(second), "coder", first, WorkspaceIdentity(strings.ToUpper(first))} {
		if client, err := login(user, firstKey); err == nil {
			client.Close()
			t.Fatalf("gateway accepted the first workspace's key for login %q", user)
		}
	}
	client, err := login(WorkspaceIdentity(first), firstKey)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	own := net.JoinHostPort(WorkspaceIdentity(first), "2222")
	if name, err := reach(client, own); err != nil || name != "first" {
		t.Fatalf("authorized workspace was not reachable: %q %v", name, err)
	}
	for _, destination := range []string{net.JoinHostPort(WorkspaceIdentity(second), "2222"), net.JoinHostPort(WorkspaceIdentity(first), "22"), secondEndpoint.String(), "localhost:2222"} {
		if name, err := reach(client, destination); err == nil {
			t.Fatalf("gateway forwarded %s to %q for the first workspace's key", destination, name)
		}
	}
	if session, err := client.NewSession(); err == nil {
		session.Close()
		t.Fatal("gateway exposed a shell")
	}
	if forward, err := client.Listen("tcp", "127.0.0.1:0"); err == nil {
		forward.Close()
		t.Fatal("gateway accepted a remote forward")
	}
	other, err := login(WorkspaceIdentity(second), secondKey)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if name, err := reach(other, net.JoinHostPort(WorkspaceIdentity(second), "2222")); err != nil || name != "second" {
		t.Fatalf("second workspace was not reachable with its own key: %q %v", name, err)
	}

	resolver.revoke(first)
	if name, err := reach(client, own); err == nil {
		t.Fatalf("authenticated connection opened a tunnel to %q after its key was revoked", name)
	}
}
