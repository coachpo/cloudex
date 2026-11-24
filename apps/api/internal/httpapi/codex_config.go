package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/pelletier/go-toml/v2"
)

const (
	maxCodexInstructionsBytes = 32 * 1024
	maxCodexTOMLBytes         = 32 * 1024
	maxCodexAuthBytes         = 64 * 1024
)

func (s *Server) getCodexConfig(w http.ResponseWriter, r *http.Request) {
	config, err := s.Store.CodexConfig(r.Context(), workspaceID(r))
	if config.Revision == 0 {
		config.ConfigTOML = runtime.CodexConfigSeed
	}
	send(w, r, config, err)
}

func (s *Server) setCodexConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Managed    json.RawMessage `json:"managed"`
		Revision   json.RawMessage `json:"revision"`
		AgentsMD   json.RawMessage `json:"agents_md"`
		ConfigTOML json.RawMessage `json:"config_toml"`
		AuthJSON   json.RawMessage `json:"auth_json"`
	}
	// JSON can encode each byte of the files as a six-byte escape.
	const maxBody = 6*(maxCodexInstructionsBytes+maxCodexTOMLBytes+maxCodexAuthBytes) + 8*1024
	if !parseWithLimit(w, r, &in, false, maxBody) {
		return
	}
	invalid := func(message string) {
		problem(w, r, http.StatusBadRequest, "INVALID_CODEX_CONFIG", message)
	}
	var settings store.CodexConfigSettings
	var revision int64
	if !decodeCodexField(in.Managed, &settings.Managed) || !decodeCodexField(in.Revision, &revision) || revision < 0 {
		invalid("managed must be a boolean and revision must be a non-negative integer.")
		return
	}
	if !decodeCodexField(in.AgentsMD, &settings.AgentsMD) || !decodeCodexField(in.ConfigTOML, &settings.ConfigTOML) ||
		!decodeCodexField(in.AuthJSON, &settings.AuthJSON) {
		invalid("Provide agents_md, config_toml, and auth_json as strings.")
		return
	}
	if len(settings.AgentsMD) > maxCodexInstructionsBytes || strings.ContainsRune(settings.AgentsMD, 0) {
		invalid("AGENTS.md must be at most 32 KiB and contain no NUL characters.")
		return
	}
	if len(settings.ConfigTOML) > maxCodexTOMLBytes || strings.ContainsRune(settings.ConfigTOML, 0) {
		invalid("config.toml must be at most 32 KiB and contain no NUL characters.")
		return
	}
	var configTOML map[string]any
	if err := toml.Unmarshal([]byte(settings.ConfigTOML), &configTOML); err != nil {
		invalid("config.toml must be valid TOML.")
		return
	}
	if value, exists := configTOML["cli_auth_credentials_store"]; exists && value != "file" {
		invalid("cli_auth_credentials_store must be the string \"file\" when specified in config.toml.")
		return
	}
	if len(settings.AuthJSON) > maxCodexAuthBytes || strings.ContainsRune(settings.AuthJSON, 0) {
		invalid("auth.json must be at most 64 KiB and contain no NUL characters.")
		return
	}
	if settings.AuthJSON != "" {
		if !json.Valid([]byte(settings.AuthJSON)) {
			invalid("auth.json must be valid JSON or an empty string.")
			return
		}
		if !strings.HasPrefix(strings.TrimLeft(settings.AuthJSON, " \t\r\n"), "{") {
			invalid("auth.json must contain a JSON object.")
			return
		}
	}
	config, err := s.Store.SaveCodexConfig(r.Context(), workspaceID(r), revision, settings)
	if errors.Is(err, store.ErrCodexConfigChanged) {
		problem(w, r, http.StatusConflict, "CODEX_CONFIG_CHANGED", err.Error())
		return
	}
	send(w, r, config, err)
}

func decodeCodexField[T any](raw json.RawMessage, target *T) bool {
	var value *T
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return false
	}
	*target = *value
	return true
}
