package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/distribution/reference"
)

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.Settings(r.Context())
	if err != nil {
		failure(w, r, err)
		return
	}
	send(w, r, map[string]any{
		"workspace_image":      settings.WorkspaceImage,
		"ssh_host":             settings.SSHHost,
		"ssh_bind":             s.SSH.Bind.String(),
		"agent_public_address": settings.AgentPublicAddress,
	}, nil)
}

func (s *Server) setWorkspaceImage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Image string `json:"image"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	image := strings.TrimSpace(in.Image)
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		problem(w, r, http.StatusBadRequest, "INVALID_WORKSPACE_IMAGE", "Enter a valid Docker image reference.")
		return
	}
	err := s.Store.SetWorkspaceImage(r.Context(), image)
	send(w, r, map[string]string{"workspace_image": image}, err)
}

func (s *Server) setAccessSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SSHHost            *string `json:"ssh_host"`
		AgentPublicAddress *string `json:"agent_public_address"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	if in.SSHHost == nil || in.AgentPublicAddress == nil {
		bad(w, r, "Send both the SSH hostname and the Agent public address. Use an empty string to clear a field.")
		return
	}
	if *in.SSHHost != "" && !remote.ValidHost(*in.SSHHost) {
		problem(w, r, http.StatusBadRequest, "INVALID_SSH_HOST", "Enter a DNS name or IPv4 address for the SSH hostname.")
		return
	}
	if *in.AgentPublicAddress != "" && !validAgentPublicAddress(*in.AgentPublicAddress) {
		problem(w, r, http.StatusBadRequest, "INVALID_AGENT_PUBLIC_ADDRESS", "Enter a hostname or IP address with a port from 1 to 65535.")
		return
	}
	err := s.Store.SetAccessSettings(r.Context(), *in.SSHHost, *in.AgentPublicAddress)
	send(w, r, map[string]string{"ssh_host": *in.SSHHost, "agent_public_address": *in.AgentPublicAddress}, err)
}

func validAgentPublicAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return false
	}
	if strings.HasPrefix(address, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	// The Agent's DNS resolver rejects names made of only digits and dots.
	if strings.Trim(host, "0123456789.") == "" {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is4()
	}
	return remote.ValidHost(host)
}
