package httpapi

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func validCredential(value string, maxLength int) bool {
	if len(value) == 0 || len(value) > maxLength {
		return false
	}
	for i := range len(value) {
		if value[i] < '!' || value[i] > '~' {
			return false
		}
	}
	return true
}

type repositoryCredentialInput struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

func (in repositoryCredentialInput) validate(w http.ResponseWriter, r *http.Request, host string) *store.RepositoryCredential {
	switch {
	case host == "":
		bad(w, r, "Configure an HTTPS repository URL before saving credentials.")
	case !validCredential(in.Username, 256) || strings.ContainsRune(in.Username, ':'):
		bad(w, r, "Enter a repository username of 1–256 visible ASCII characters without a colon.")
	case !validCredential(in.Token, 4096):
		bad(w, r, "Enter a repository token of 1–4096 visible ASCII characters without whitespace.")
	default:
		return &store.RepositoryCredential{Host: host, Username: in.Username, Token: in.Token}
	}
	return nil
}

func (s *Server) getRepositoryCredential(w http.ResponseWriter, r *http.Request) {
	status, err := s.Store.RepositoryCredentialStatus(r.Context(), workspaceID(r))
	send(w, r, status, err)
}

func (s *Server) putRepositoryCredential(w http.ResponseWriter, r *http.Request) {
	var in repositoryCredentialInput
	if !parse(w, r, &in, false) {
		return
	}
	ws, err := s.Store.Workspace(r.Context(), workspaceID(r))
	if err != nil {
		failure(w, r, err)
		return
	}
	repo, err := url.Parse(ws.RepoURL)
	if err != nil || repo.Scheme != "https" {
		bad(w, r, "Configure an HTTPS repository URL before saving credentials.")
		return
	}
	credential := in.validate(w, r, repo.Host)
	if credential == nil {
		return
	}
	err = s.Store.SetRepositoryCredential(r.Context(), ws.ID, credential)
	send(w, r, store.RepositoryCredentialStatus{Configured: true, Username: credential.Username}, err)
}

func (s *Server) deleteRepositoryCredential(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.SetRepositoryCredential(r.Context(), workspaceID(r), nil); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
