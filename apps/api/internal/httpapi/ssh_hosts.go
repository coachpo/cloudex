package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/ssh"
)

type sshHostConnections interface {
	Validate(context.Context, hostssh.Config) error
	Changed(string)
	Retry(string)
}

func validSSHAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	n, errPort := strconv.Atoi(port)
	return err == nil && errPort == nil && n >= 1 && n <= 65535 && host != "" && !strings.ContainsAny(host, " \t\r\n/@")
}

func (s *Server) probeSSHHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address string `json:"address"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	in.Address = strings.TrimSpace(in.Address)
	if !validSSHAddress(in.Address) {
		bad(w, r, "Enter the SSH host and port.")
		return
	}
	key, err := hostssh.Probe(r.Context(), in.Address)
	if err != nil {
		problem(w, r, http.StatusBadGateway, "SSH_PROBE_FAILED", "Could not read the SSH host key. Check the address and SSH service.")
		return
	}
	reply(w, r, http.StatusOK, map[string]string{"host_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "fingerprint": ssh.FingerprintSHA256(key)})
}

func (s *Server) saveSSHHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		Address    string `json:"address"`
		Username   string `json:"username"`
		HostKey    string `json:"host_key"`
		Password   string `json:"password"`
		PrivateKey string `json:"private_key"`
		Passphrase string `json:"passphrase"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	id := hostID(r)
	in.Name = strings.TrimSpace(in.Name)
	if id == "" && !workspaceName.MatchString(in.Name) {
		bad(w, r, "Use 1–64 letters, digits, spaces, dots, underscores or hyphens for the Host name.")
		return
	}
	cfg := hostssh.Config{Address: strings.TrimSpace(in.Address), User: strings.TrimSpace(in.Username), HostKey: strings.TrimSpace(in.HostKey), Password: in.Password, PrivateKey: in.PrivateKey, Passphrase: in.Passphrase}
	if !validSSHAddress(cfg.Address) || cfg.User == "" {
		bad(w, r, "Enter the SSH host, port and username.")
		return
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
	if err != nil || len(rest) != 0 {
		bad(w, r, "Confirm the SSH host public key.")
		return
	}
	cfg.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if (cfg.Password == "") == (cfg.PrivateKey == "") || cfg.Passphrase != "" && cfg.PrivateKey == "" {
		bad(w, r, "Provide a password or a private key with its optional passphrase.")
		return
	}
	if s.SSHHosts == nil || s.Key == nil {
		problem(w, r, 503, "SSH_UNAVAILABLE", "SSH Host connections are unavailable.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if id != "" {
		unlock, err := s.lockHostMode(ctx, id)
		if err != nil {
			failure(w, r, err)
			return
		}
		defer unlock()
		h, err := s.Store.Host(ctx, id)
		if err != nil {
			failure(w, r, err)
			return
		}
		if h.ConnectionType != "ssh" || h.SSHAddress != cfg.Address || h.SSHUser != cfg.User {
			bad(w, r, "Use the registered SSH Host address and username.")
			return
		}
	}
	if err := s.SSHHosts.Validate(ctx, cfg); err != nil {
		if errors.Is(err, hostssh.ErrHostKeyChanged) {
			problem(w, r, 409, "SSH_HOST_KEY_CHANGED", "SSH host key changed. Verify the current fingerprint before updating credentials.")
			return
		}
		problem(w, r, http.StatusUnprocessableEntity, "SSH_CONNECTION_FAILED", err.Error())
		return
	}
	var host store.Host
	status := http.StatusOK
	if id == "" {
		host, err = s.Store.AddSSHHost(ctx, in.Name, cfg, s.Key)
		status = http.StatusCreated
	} else {
		host, err = s.Store.UpdateSSHHost(ctx, id, cfg, s.Key)
	}
	if err != nil {
		failure(w, r, err)
		return
	}
	s.SSHHosts.Changed(host.ID)
	w.Header().Set("Location", "/api/v1/hosts/"+host.ID)
	reply(w, r, status, host)
}

func (s *Server) reconnectSSHHost(w http.ResponseWriter, r *http.Request) {
	id := hostID(r)
	ctx := r.Context()
	unlock, err := s.lockHostMode(ctx, id)
	if err != nil {
		failure(w, r, err)
		return
	}
	defer unlock()
	h, err := s.Store.Host(ctx, id)
	if err != nil {
		failure(w, r, err)
		return
	}
	if h.ConnectionType != "ssh" {
		bad(w, r, "Select an SSH Host.")
		return
	}
	if h.Scheduling == "disabled" {
		failure(w, r, store.ErrHostDisabled)
		return
	}
	if s.SSHHosts == nil {
		problem(w, r, 503, "SSH_UNAVAILABLE", "SSH Host connections are unavailable.")
		return
	}
	if h.Status != "online" {
		s.SSHHosts.Retry(id)
	}
	reply(w, r, 200, h)
}
