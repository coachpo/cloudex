package httpapi

import (
	"net/http"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/pelletier/go-toml/v2"
)

const maxInitialCodexFileBytes = 32 * 1024

func (s *Server) codexFiles(w http.ResponseWriter, r *http.Request) {
	files, err := s.Store.InitialCodexFiles(r.Context())
	send(w, r, files, err)
}

func (s *Server) setCodexFiles(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentsMD   *string `json:"agents_md"`
		ConfigTOML *string `json:"config_toml"`
	}
	// JSON can encode each byte of the files as a six-byte escape.
	const maxBody = 6*2*maxInitialCodexFileBytes + 8*1024
	if !parseWithLimit(w, r, &in, false, maxBody) {
		return
	}
	invalid := func(message string) {
		problem(w, r, http.StatusBadRequest, "INVALID_CODEX_CONFIG", message)
	}
	if in.AgentsMD == nil || in.ConfigTOML == nil {
		invalid("Provide agents_md and config_toml as strings.")
		return
	}
	files := store.InitialCodexFiles{AgentsMD: *in.AgentsMD, ConfigTOML: *in.ConfigTOML}
	if len(files.AgentsMD) > maxInitialCodexFileBytes || strings.ContainsRune(files.AgentsMD, 0) {
		invalid("AGENTS.md must be at most 32 KiB and contain no NUL characters.")
		return
	}
	if len(files.ConfigTOML) > maxInitialCodexFileBytes || strings.ContainsRune(files.ConfigTOML, 0) {
		invalid("config.toml must be at most 32 KiB and contain no NUL characters.")
		return
	}
	var configTOML map[string]any
	if err := toml.Unmarshal([]byte(files.ConfigTOML), &configTOML); err != nil {
		invalid("config.toml must be valid TOML.")
		return
	}
	if value, exists := configTOML["cli_auth_credentials_store"]; exists && value != "file" {
		invalid("cli_auth_credentials_store must be the string \"file\" when specified in config.toml.")
		return
	}
	send(w, r, files, s.Store.SetInitialCodexFiles(r.Context(), files))
}
