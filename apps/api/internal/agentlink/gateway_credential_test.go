package agentlink

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/ssh"
)

// The controlled SSH service accepts only the fixed Docker socket command and
// relays its channel to the disposable daemon. No machine account is needed.
func fixtureDockerSSH(t *testing.T, socket string) *hostssh.Client {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.ServerConfig{PasswordCallback: func(m ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if m.User() != "fixture" || string(password) != "synthetic-password" {
			return nil, errors.New("SSH fixture credential mismatch")
		}
		return nil, nil
	}}
	server.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var done sync.WaitGroup
	done.Go(func() {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		conn, channels, requests, err := ssh.NewServerConn(raw, server)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		var sessions sync.WaitGroup
		for incoming := range channels {
			sessions.Go(func() { fixtureDockerSession(t, incoming, socket) })
		}
		sessions.Wait()
	})
	t.Cleanup(func() { listener.Close(); done.Wait() })
	client, err := hostssh.Connect(t.Context(), hostssh.Config{
		Address: listener.Addr().String(), User: "fixture", Password: "synthetic-password",
		HostKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func fixtureDockerSession(t *testing.T, incoming ssh.NewChannel, socket string) {
	if incoming.ChannelType() != "session" {
		_ = incoming.Reject(ssh.UnknownChannelType, "only Docker CLI sessions allowed")
		return
	}
	channel, requests, err := incoming.Accept()
	if err != nil {
		return
	}
	defer channel.Close()
	request := <-requests
	if request == nil {
		return
	}
	var command struct{ Command string }
	if request.Type != "exec" || ssh.Unmarshal(request.Payload, &command) != nil ||
		command.Command != "docker --host=unix:///var/run/docker.sock system dial-stdio" {
		t.Error("SSH client did not use the reserved Docker socket command")
		_ = request.Reply(false, nil)
		return
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Errorf("cannot connect to the isolated Docker socket: %v", err)
		_ = request.Reply(false, nil)
		return
	}
	defer conn.Close()
	if request.Reply(true, nil) != nil {
		return
	}
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		_, _ = io.Copy(conn, channel)
		_ = conn.CloseWrite()
	}()
	_, _ = io.Copy(channel, conn)
	_ = channel.Close()
	<-stdinDone
}

func TestGatewayCredentialHostRoutes(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_DOCKER") != "1" {
		t.Skip("set CLOUDEX_TEST_DOCKER=1 with an isolated daemon and built Workspace image")
	}
	address := os.Getenv("DOCKER_HOST")
	if address == "" {
		address = "unix:///var/run/docker.sock"
	}
	socket, err := url.Parse(address)
	if err != nil || socket.Scheme != "unix" || socket.Path == "" {
		t.Fatal("DOCKER_HOST must identify the isolated Unix Docker socket")
	}
	for _, route := range []string{"agent-mtls", "ssh-direct"} {
		t.Run(route, func(t *testing.T) {
			instance := store.ID()
			// Docker's fixture SSH relay connects to the same isolated Linux daemon
			// as the Agent. Each route owns a distinct set of runtime resources.
			docker, err := runtime.NewDocker(instance)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = docker.Close() })
			network := os.Getenv("CLOUDEX_TEST_SSH_NETWORK")
			if network == "" {
				t.Fatal("CLOUDEX_TEST_SSH_NETWORK must identify the disposable Workspace test bridge")
			}
			if err := docker.EnsureSSHNetwork(t.Context(), network); err != nil {
				t.Fatal(err)
			}
			var engine runtime.Engine
			if route == "agent-mtls" {
				hub, server, beats := testHub(t, nil)
				startTestAgent(t, server, "gateway-host", Agent{
					Engine: docker, JournalPath: filepath.Join(t.TempDir(), "commands.sqlite"), ValidateContainer: docker.ValidateContainer,
				})
				awaitHeartbeat(t, beats, "gateway-host")
				engine = hub.Engine("gateway-host")
			} else {
				client := fixtureDockerSSH(t, socket.Path)
				remote, err := runtime.NewRemoteDocker(instance, "gateway-host", client.DialDocker, client.DialContext)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = remote.Close() })
				engine = remote
			}
			verifyGatewayCredentialHostRoute(t, engine, network)
		})
	}
}

func verifyGatewayCredentialHostRoute(t *testing.T, engine runtime.Engine, network string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	target := runtime.Spec{WorkspaceID: store.ID(), Generation: 1, CreationOperationID: store.ID(), Image: "cloudex/workspace:local", SSHNetwork: network, CPU: 500, Memory: 512}
	created, err := engine.Ensure(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := engine.Remove(cleanup, target); err != nil {
			t.Error(err)
		}
		if err := engine.Purge(cleanup, target, true); err != nil {
			t.Error(err)
		}
	})
	start := func() runtime.GatewayInstance {
		t.Helper()
		if err := engine.Start(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		c, err := engine.Inspect(ctx, target)
		if err != nil || c.ID != created.ID || c.State != "running" || c.StartedAt == "" {
			t.Fatalf("expected the running instance: %v, state %q", err, c.State)
		}
		return runtime.GatewayInstance{ContainerID: c.ID, StartedAt: c.StartedAt}
	}
	inspector := engine.(runtime.GatewayCredentialRuntime)
	awaitEmpty := func(instance runtime.GatewayInstance) {
		t.Helper()
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		for {
			result, err := inspector.InspectGatewayCredential(check, target, instance)
			if err == nil {
				if result.Sequence != 0 || result.Digest != "" || result.StartedAt != instance.StartedAt {
					t.Fatal("the new startup did not begin with an empty credential mount")
				}
				return
			}
			select {
			case <-check.Done():
				t.Fatalf("could not inspect the startup credential state: %v", err)
			case <-time.After(45 * time.Millisecond):
			}
		}
	}
	first := start()
	awaitEmpty(first)
	install := runtime.GatewayCredentialInstall{
		Origin: "https://gateway.test", BindingRevision: 1, GrantEpoch: 1, Sequence: 1,
		Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
	}
	verify := func(credential runtime.GatewayCredentialInstall) {
		t.Helper()
		digest := sha256.Sum256([]byte(credential.Token))
		expected := runtime.GatewayCredentialConfirmation{
			Digest: hex.EncodeToString(digest[:]), StartedAt: first.StartedAt, Origin: credential.Origin,
			BindingRevision: credential.BindingRevision, GrantEpoch: credential.GrantEpoch, Sequence: credential.Sequence,
		}
		confirm, err := inspector.InstallGatewayCredential(ctx, target, first, credential)
		if err != nil || confirm != expected {
			t.Fatalf("credential was not confirmed for sequence %d: %v", credential.Sequence, err)
		}
		if got, err := inspector.InspectGatewayCredential(ctx, target, first); err != nil || got != expected {
			t.Fatalf("the running instance did not retain sequence %d: %v", credential.Sequence, err)
		}
	}
	verify(install)
	verify(install) // replay a lost confirmation
	rotated := install
	rotated.Sequence++
	rotated.Token = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	verify(rotated)
	if _, err := inspector.InstallGatewayCredential(ctx, target, first, install); !errors.Is(err, runtime.ErrOwnership) {
		t.Fatalf("stale installation did not lose write permission: %v", err)
	}
	var output bytes.Buffer
	exit, err := engine.Run(ctx, first.ContainerID, runtime.Exec{
		Args: []string{"sh", "-c", `test -r /run/cloudex/gateway.json && stat -c '%u:%g:%a' /run/cloudex/gateway.json && codex --version`},
		Dir:  "/workspace", Stdout: &output,
	})
	if err != nil || exit != 0 || strings.TrimSpace(output.String()) != "0:10001:440\ncodex-cli "+codexupstream.CodexClientVersion {
		t.Fatalf("credential mode or image entry point failed: exit=%d err=%v output=%q", exit, err, output.String())
	}
	if err := engine.Stop(ctx, first.ContainerID); err != nil {
		t.Fatal(err)
	}
	second := start()
	if second.StartedAt == first.StartedAt {
		t.Fatal("Stop / Start reused the instance's startup marker")
	}
	if _, err := inspector.InspectGatewayCredential(ctx, target, first); !errors.Is(err, runtime.ErrOwnership) {
		t.Fatalf("old startup retained credential confirmation: %v", err)
	}
	awaitEmpty(second)
	t.Logf("Confirmed exact Docker instance, install replay and rotation, readable credential mode, image Codex %s, and tmpfs clearing after Stop / Start", codexupstream.CodexClientVersion)
}
