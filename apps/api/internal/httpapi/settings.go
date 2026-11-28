package httpapi

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/distribution/reference"
	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.Settings(r.Context())
	if err != nil {
		failure(w, r, err)
		return
	}
	codex, err := s.Store.CodexSettings(r.Context())
	if err != nil {
		failure(w, r, err)
		return
	}
	send(w, r, map[string]any{
		"workspace_image":      settings.WorkspaceImage,
		"ssh_host":             settings.SSHHost,
		"ssh_bind":             s.SSH.Bind.String(),
		"agent_public_address": settings.AgentPublicAddress,
		"codex_config_toml":    codex.ConfigTOML,
		"codex_agents_md":      codex.AgentsMD,
	}, nil)
}

func (s *Server) setCodexSettings(w http.ResponseWriter, r *http.Request) {
	const maxFileBytes = 32 * 1024
	// JSON can encode each byte of the files as a six-byte escape.
	const maxBody = 6*2*maxFileBytes + 8*1024
	invalid := func(message string) {
		problem(w, r, http.StatusBadRequest, "INVALID_CODEX_SETTINGS", message)
	}
	var in struct {
		ConfigTOML json.RawMessage `json:"config_toml"`
		AgentsMD   json.RawMessage `json:"agents_md"`
	}
	if !parseWithLimit(w, r, &in, false, maxBody, func() {
		invalid("config.toml and AGENTS.md must each be at most 32 KiB.")
	}) {
		return
	}
	var configTOML, agentsMD *string
	if err := json.Unmarshal(in.ConfigTOML, &configTOML); err != nil || configTOML == nil {
		invalid("config_toml must be a string.")
		return
	}
	if err := json.Unmarshal(in.AgentsMD, &agentsMD); err != nil || agentsMD == nil {
		invalid("agents_md must be a string.")
		return
	}
	if len(*configTOML) > maxFileBytes || strings.ContainsRune(*configTOML, 0) {
		invalid("config.toml must be at most 32 KiB and contain no NUL characters.")
		return
	}
	if len(*agentsMD) > maxFileBytes || strings.ContainsRune(*agentsMD, 0) {
		invalid("AGENTS.md must be at most 32 KiB and contain no NUL characters.")
		return
	}
	var config map[string]any
	configBytes := []byte(*configTOML)
	if err := toml.Unmarshal(configBytes, &config); err != nil || hasUnsupportedCodexTOML(configBytes) {
		invalid("config.toml must be valid TOML.")
		return
	}
	for _, key := range []string{"model_provider", "model_providers", "model_catalog_json",
		"cli_auth_credentials_store", "openai_base_url", "chatgpt_base_url"} {
		if _, exists := config[key]; exists {
			invalid("config.toml cannot set the reserved key " + key + ".")
			return
		}
	}
	settings := store.CodexSettings{ConfigTOML: *configTOML, AgentsMD: *agentsMD}
	send(w, r, settings, s.Store.SetCodexSettings(r.Context(), settings))
}

func hasUnsupportedCodexTOML(config []byte) bool {
	// go-toml accepts \e and year zero, which the launcher's tomllib rejects.
	if !bytes.Contains(config, []byte(`\e`)) && !bytes.Contains(config, []byte("0000-")) {
		return false
	}
	var parser unstable.Parser
	parser.Reset(config)
	for parser.NextExpression() {
		if hasUnsupportedCodexTOMLNode(&parser, parser.Expression()) {
			return true
		}
	}
	return false
}

func hasUnsupportedCodexTOMLNode(parser *unstable.Parser, node *unstable.Node) bool {
	switch node.Kind {
	case unstable.LocalDate, unstable.LocalDateTime, unstable.DateTime:
		return bytes.HasPrefix(node.Data, []byte("0000-"))
	case unstable.String, unstable.Key:
		raw := parser.Raw(node.Raw)
		if len(raw) > 0 && raw[0] == '"' {
			for i := bytes.IndexByte(raw, '\\'); i >= 0 && i+1 < len(raw); i = bytes.IndexByte(raw, '\\') {
				if raw[i+1] == 'e' {
					return true
				}
				raw = raw[i+2:]
			}
		}
	}
	for children := node.Children(); children.Next(); {
		if hasUnsupportedCodexTOMLNode(parser, children.Node()) {
			return true
		}
	}
	return false
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
