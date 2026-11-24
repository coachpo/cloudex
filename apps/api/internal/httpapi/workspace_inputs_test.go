package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestWorkspaceCreationRejectsInvalidRepositoryBeforeAllocation(t *testing.T) {
	db := newDB(t)
	api := newTestAPI(t, &control.Control{Store: db})
	api.login()
	for _, tc := range []struct {
		name, url, branch string
	}{
		{"reserved branch", "", "HEAD"},
		{"port out of range", "https://git.example.invalid:65536/project.git", "main"},
		{"port zero", "https://git.example.invalid:0/project.git", "main"},
		{"non-numeric port", "https://git.example.invalid:unknown/project.git", "main"},
		{"empty fragment", "https://git.example.invalid/project.git#", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api.call(http.MethodPost, "/workspaces", map[string]any{
				"name":       tc.name,
				"repository": map[string]string{"url": tc.url, "branch": tc.branch},
			}, http.StatusBadRequest, nil)
		})
	}
	var workspaces []store.Workspace
	api.call(http.MethodGet, "/workspaces", nil, http.StatusOK, &workspaces)
	if len(workspaces) != 0 {
		t.Fatalf("invalid requests created Workspace records: %+v", workspaces)
	}
	var operations []store.Operation
	api.call(http.MethodGet, "/operations", nil, http.StatusOK, &operations)
	if len(operations) != 0 {
		t.Fatalf("invalid requests queued lifecycle operations: %+v", operations)
	}
	api.call(http.MethodPost, "/workspaces", map[string]any{
		"name":       "valid repository",
		"repository": map[string]string{"url": "https://git.example.invalid:8443/project.git", "branch": "head"},
	}, http.StatusAccepted, nil)
}

func TestRepositoryCredentialInputValidation(t *testing.T) {
	a := newTestAPI(t, &control.Control{Store: newDB(t)})
	a.login()
	const repo = "https://git.example.test/repo"
	for _, tc := range []struct {
		name, url, username, token string
	}{
		{name: "missing repository", username: "git", token: "synthetic-token"},
		{name: "SSH repository", url: "ssh://git.example.test/repo", username: "git", token: "synthetic-token"},
		{name: "empty username", url: repo, token: "synthetic-token"},
		{name: "username colon", url: repo, username: "git:fixture", token: "synthetic-token"},
		{name: "username space", url: repo, username: "git fixture", token: "synthetic-token"},
		{name: "username too long", url: repo, username: strings.Repeat("u", 257), token: "synthetic-token"},
		{name: "empty token", url: repo, username: "git"},
		{name: "token whitespace", url: repo, username: "git", token: "synthetic\ntoken"},
		{name: "token Unicode", url: repo, username: "git", token: "synthetic-tokén"},
		{name: "token too long", url: repo, username: "git", token: strings.Repeat("x", 4097)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a.call("POST", "/workspaces", map[string]any{
				"name":       "credential fixture",
				"repository": map[string]any{"url": tc.url, "credential": repositoryCredentialInput{Username: tc.username, Token: tc.token}},
			}, 400, nil)
		})
	}
	a.call("POST", "/workspaces", map[string]any{
		"name":       "credential fixture",
		"repository": map[string]any{"url": repo, "credential": repositoryCredentialInput{Username: strings.Repeat("u", 256), Token: strings.Repeat("~", 4096)}},
	}, 202, nil)
}
