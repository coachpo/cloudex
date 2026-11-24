package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/hostmanager"
	"github.com/coachpo/cloudex/apps/api/internal/httpapi"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/net/idna"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var browserDomain = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.CheckHyphens(false), idna.StrictDomainName(false))
var ipv4HostSuffix = regexp.MustCompile(`(?:^|\.)(?:[0-9]+|0x[0-9a-f]*)\.?$`)

func browserOrigin(value string) (string, error) {
	invalid := errors.New("CLOUDEX_ORIGIN must be a browser origin, e.g. http://localhost:3000")
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return "", invalid
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", invalid
		}
		host = addr.String()
		if addr.Is4In6() {
			// Browsers serialize the embedded IPv4 address as IPv6 hex pieces.
			v4 := addr.As4()
			host = fmt.Sprintf("::ffff:%x:%x", uint16(v4[0])<<8|uint16(v4[1]), uint16(v4[2])<<8|uint16(v4[3]))
		}
		host = "[" + host + "]"
	} else {
		host, err = browserDomain.ToASCII(host)
		if err != nil || host == "" || strings.ContainsAny(host, " #%/:<>?@[\\]^|") {
			return "", invalid
		}
		// Browsers also interpret numeric short forms and alternate bases as
		// IPv4. Require the dotted-decimal spelling sent in their Origin.
		if ipv4HostSuffix.MatchString(host) {
			if _, err := netip.ParseAddr(host); err != nil {
				return "", errors.New("CLOUDEX_ORIGIN must use a canonical dotted-decimal IPv4 address, e.g. http://127.0.0.1:3000")
			}
		}
	}
	if rawPort := u.Port(); rawPort != "" {
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil {
			return "", invalid
		}
		if (u.Scheme != "http" || port != 80) && (u.Scheme != "https" || port != 443) {
			host += ":" + strconv.FormatUint(port, 10)
		}
	}
	return u.Scheme + "://" + host, nil
}

const defaultDBPath = "data/cloud.sqlite"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := runCommand(ctx, os.Args[1:]); e != nil {
		slog.Error("container cloud", "error", e)
		os.Exit(1)
	}
}
func runCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "reset-admin" {
		return run(ctx)
	}
	if len(args) != 1 {
		return errors.New("usage: cloudex-server reset-admin")
	}
	if err := store.ResetAdmin(ctx, env("CLOUDEX_DB_PATH", defaultDBPath)); err != nil {
		return err
	}
	fmt.Println("Administrator reset. Open the web console to create a new account.")
	return nil
}

func run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	key, e := hex.DecodeString(os.Getenv("CLOUDEX_MASTER_KEY"))
	if e != nil || len(key) != 32 {
		return errors.New("CLOUDEX_MASTER_KEY must be 32 bytes encoded as 64 hex characters")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	origin, e := browserOrigin(env("CLOUDEX_ORIGIN", "http://localhost:3000"))
	if e != nil {
		return e
	}
	target, e := url.Parse(env("CLOUDEX_WEB_URL", "http://127.0.0.1:3001"))
	if e != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return errors.New("CLOUDEX_WEB_URL must be an absolute HTTP URL")
	}
	ssh, e := remote.NewConfig(env("CLOUDEX_SSH_BIND", "127.0.0.1"), env("CLOUDEX_SSH_PORT", "2222"), env("CLOUDEX_SSH_NETWORK", "cloudex-workspaces"))
	if e != nil {
		return e
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		slog.Warn("web console unavailable", "error", e)
		http.Error(w, "Web console unavailable", http.StatusBadGateway)
	}

	db, e := store.Open(ctx, env("CLOUDEX_DB_PATH", defaultDBPath))
	if e != nil {
		return e
	}
	defer db.Close()
	instance, e := db.Instance(ctx)
	if e != nil {
		return e
	}
	controller := &control.Control{Store: db, Key: aead, SSH: ssh}
	agents, e := startAgentServices(ctx, db, instance, aead, controller.SyncConnectedHostSSHKeys)
	if e != nil {
		return e
	}
	defer agents.close()
	hosts, e := hostmanager.New(ctx, db, instance, ssh.Network, aead, agents.Hub, controller.SyncConnectedHostSSHKeys)
	if e != nil {
		return e
	}
	defer hosts.Close()
	controller.Agents = hosts
	signer, e := controller.PrepareGateway(ctx, instance)
	if e != nil {
		return e
	}
	gateway, e := net.Listen("tcp", env("CLOUDEX_GATEWAY_ADDR", net.JoinHostPort(ssh.Bind.String(), strconv.Itoa(ssh.Port))))
	if e != nil {
		return fmt.Errorf("listen for SSH Gateway: %w", e)
	}
	defer gateway.Close()
	api := &httpapi.Server{Control: controller, Origin: origin, AgentCertificate: agents.caPEM,
		DisconnectAgent: agents.Disconnect, SSHHosts: hosts}
	srv := &http.Server{Addr: env("CLOUDEX_ADDR", "127.0.0.1:3000"), Handler: api.Handler(proxy),
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	workerExited, httpExited := make(chan error, 1), make(chan error, 1)
	gatewayExited := make(chan error, 1)
	go func() {
		slog.Info("SSH Gateway listening", "address", gateway.Addr())
		gatewayExited <- remote.Serve(ctx, gateway, signer, controller)
	}()
	go func() { workerExited <- controller.Run(ctx) }()
	go func() { slog.Info("listening", "address", srv.Addr); httpExited <- srv.ListenAndServe() }()
	var exit error
	select {
	case <-ctx.Done():
	case e := <-workerExited:
		workerExited = nil
		if e != nil {
			exit = fmt.Errorf("workers: %w", e)
		}
	case e := <-httpExited:
		if !errors.Is(e, http.ErrServerClosed) {
			exit = e
		}
	case e := <-gatewayExited:
		gatewayExited = nil
		if e != nil {
			exit = fmt.Errorf("SSH Gateway: %w", e)
		}
	case e := <-agents.exited:
		if !errors.Is(e, http.ErrServerClosed) {
			exit = fmt.Errorf("Host Agent listener: %w", e)
		}
	}
	cancel()
	stop, end := context.WithTimeout(context.Background(), 10*time.Second)
	defer end()
	_ = srv.Shutdown(stop)
	if workerExited != nil {
		<-workerExited
	}
	if gatewayExited != nil {
		<-gatewayExited
	}
	return exit
}
