package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/agentauth"
	"github.com/coachpo/cloudex/apps/api/internal/agentlink"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

const initialMaxOperations = 3

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("Host Agent", "error", err)
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func run(ctx context.Context) error {
	cfg := agentauth.Client{
		Address: env("CLOUDEX_AGENT_ADDRESS", "127.0.0.1:9443"), Socket: os.Getenv("CLOUDEX_AGENT_BOOTSTRAP_SOCKET"),
		CAFile: env("CLOUDEX_AGENT_CA_FILE", "agent-ca.pem"), StateDir: env("CLOUDEX_AGENT_STATE_DIR", "data/agent"),
		Token: os.Getenv("CLOUDEX_AGENT_ENROLLMENT_TOKEN"),
	}
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(host, "/@?# \\") {
		return errors.New("CLOUDEX_AGENT_ADDRESS must be a Host and port")
	}
	network := env("CLOUDEX_SSH_NETWORK", "cloudex-workspaces")
	identity, err := cfg.Bootstrap(ctx)
	if err != nil {
		return err
	}
	if len(identity.Instance) != 32 || identity.HostID == "" {
		return errors.New("invalid Host Agent enrollment identity")
	}
	engine, err := runtime.NewHostDocker(identity.Instance, identity.HostID)
	if err != nil {
		return err
	}
	defer engine.Close()
	agent := agentlink.Agent{
		Engine: engine, HasImage: engine.HasImage, ValidateContainer: engine.ValidateContainer, MaxConcurrent: initialMaxOperations,
		JournalPath: filepath.Join(cfg.StateDir, "commands.sqlite"),
		Inventory: func(ctx context.Context) (agentlink.Inventory, error) {
			info, err := engine.Host(ctx)
			return agentlink.Inventory{Host: info, Version: "2.5.0"}, err
		},
		DialSSH: func(ctx context.Context, id string, address netip.AddrPort) (net.Conn, error) {
			return engine.DialWorkspaceSSH(ctx, id, address, network)
		},
	}
	slog.Info("Host Agent configured", "host", identity.HostID, "address", cfg.Address)
	return runAgent(ctx, cfg, agent, identity)
}

func runAgent(ctx context.Context, cfg agentauth.Client, agent agentlink.Agent, identity agentauth.Identity) error {
	credential, err := agentauth.NewCredential(identity)
	if err != nil {
		return err
	}
	// Reconnect within seconds once the control plane is back: resolve the name
	// on every dial (gRPC's DNS resolver rate-limits re-resolution after a
	// restarted control plane's name briefly disappears), cap the backoff
	// (default 120 s), and notice a silently dropped path. A renewed
	// certificate is used by later handshakes; the open connection stays up.
	conn, err := grpc.NewClient("passthrough:///"+cfg.Address, grpc.WithTransportCredentials(credentials.NewTLS(credential.TLSConfig())),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{
			BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second}, MinConnectTimeout: 5 * time.Second}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 20 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}))
	if err != nil {
		return err
	}
	defer conn.Close()
	link, cancel := context.WithCancel(ctx)
	defer cancel()
	exited := make(chan error, 1)
	go func() { exited <- agent.Run(link, conn) }()
	renewing := make(chan struct{})
	go func() {
		defer close(renewing)
		cfg.KeepRenewed(link, identity, credential)
	}()
	var runErr error
	select {
	case runErr = <-exited:
		cancel()
	case <-ctx.Done():
		cancel()
		runErr = <-exited
	}
	<-renewing
	return runErr
}
