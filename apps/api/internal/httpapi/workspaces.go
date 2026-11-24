package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
	"github.com/go-chi/chi/v5"
)

func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.Settings(r.Context())
	send(w, r, []map[string]any{{"id": "codex", "name": "Codex · Node.js & Python", "image": settings.WorkspaceImage, "cpu_milli": 2000, "memory_mb": 4096}}, err)
}
func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	if host := r.URL.Query().Get("host_id"); host != "" {
		v, e := s.Store.HostWorkspaces(r.Context(), host)
		send(w, r, v, e)
		return
	}
	v, e := s.Store.Workspaces(r.Context())
	send(w, r, v, e)
}
func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Workspace(r.Context(), workspaceID(r))
	send(w, r, v, e)
}

var workspaceName = regexp.MustCompile(`^[\pL\pN][\pL\pN ._-]{0,63}$`)
var gitBranch = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_./-]{0,127}$`)

func validBranch(branch string) bool {
	if branch == "HEAD" || !gitBranch.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".") {
		return false
	}
	for _, p := range strings.Split(branch, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		Template   string `json:"template_id"`
		Repository struct {
			URL        string                     `json:"url"`
			Branch     string                     `json:"branch"`
			Credential *repositoryCredentialInput `json:"credential"`
		} `json:"repository"`
		Placement struct {
			Type   string `json:"type"`
			HostID string `json:"host_id"`
		} `json:"placement"`
		Resources struct {
			CPU         int64 `json:"cpu_milli"`
			Memory      int64 `json:"memory_mb"`
			MemoryBytes int64 `json:"memory_bytes"`
			Disk        int64 `json:"disk_bytes"`
		} `json:"resources"`
		AutoStart     *bool    `json:"auto_start"`
		SSHPublicKeys []string `json:"ssh_public_keys"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	if s.Agents != nil || in.Placement.HostID != "" || in.Placement.Type != "" {
		if in.Placement.Type != "manual" || in.Placement.HostID == "" {
			bad(w, r, "Select a Host for manual placement.")
			return
		}
	}
	in.Name = strings.TrimSpace(in.Name)
	if !workspaceName.MatchString(in.Name) {
		bad(w, r, "Use 1–64 letters, digits, spaces, dots, underscores or hyphens for the workspace name.")
		return
	}
	if in.Template == "" {
		in.Template = "codex"
	}
	if in.Template != "codex" {
		bad(w, r, "Unknown workspace template.")
		return
	}
	if in.Repository.Branch == "" {
		in.Repository.Branch = "main"
	}
	if !validBranch(in.Repository.Branch) {
		bad(w, r, "Enter a valid Git branch name.")
		return
	}
	in.Repository.URL = strings.TrimSpace(in.Repository.URL)
	var repoHost string
	if in.Repository.URL != "" {
		u, e := url.Parse(in.Repository.URL)
		if e != nil || len(in.Repository.URL) > 1024 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path == "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(in.Repository.URL, "#") {
			bad(w, r, "Enter an HTTPS Git repository URL without embedded credentials, query, or fragment.")
			return
		}
		if rawPort := u.Port(); rawPort != "" {
			port, err := strconv.ParseUint(rawPort, 10, 16)
			if err != nil || port == 0 {
				bad(w, r, "Enter a repository port between 1 and 65535.")
				return
			}
		}
		repoHost = u.Host
	}
	var credential *store.RepositoryCredential
	if in.Repository.Credential != nil {
		credential = in.Repository.Credential.validate(w, r, repoHost)
		if credential == nil {
			return
		}
	}
	if in.Resources.CPU == 0 {
		in.Resources.CPU = 2000
	}
	if in.Resources.MemoryBytes != 0 {
		const mib = 1024 * 1024
		if in.Resources.Memory != 0 || in.Resources.MemoryBytes < 0 || in.Resources.MemoryBytes%mib != 0 {
			bad(w, r, "Specify memory once, in whole MiB.")
			return
		}
		in.Resources.Memory = in.Resources.MemoryBytes / mib
		in.Resources.MemoryBytes = 0
	}
	if in.Resources.Memory == 0 {
		in.Resources.Memory = 4096
	}
	if in.Resources.CPU < 100 || in.Resources.CPU > 1_000_000 || in.Resources.Memory < 256 || in.Resources.Memory > 1_048_576 {
		bad(w, r, "CPU must be at least 0.1 cores and memory at least 256 MiB, within the supported range.")
		return
	}
	if in.Resources.Disk < 0 {
		bad(w, r, "Disk reservation cannot be negative.")
		return
	}
	desired := "running"
	if in.AutoStart != nil && !*in.AutoStart {
		desired = "stopped"
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 || (s.Agents != nil && strings.TrimSpace(key) == "") {
		bad(w, r, "Provide an Idempotency-Key of 1–128 characters.")
		return
	}
	ssh, e := remote.Keys(in.SSHPublicKeys)
	if e != nil {
		bad(w, r, e.Error())
		return
	}
	metadata := []any{"create", in.Name, in.Template, in.Repository.URL, in.Repository.Branch, in.Resources, desired, in.Placement.HostID}
	if credential != nil {
		metadata = append(metadata, credential.Username)
	}
	if len(ssh) > 0 {
		metadata = append(metadata, ssh)
	}
	settings, e := s.Store.Settings(r.Context())
	if e != nil {
		failure(w, r, e)
		return
	}
	ws := store.Workspace{ID: store.ID(), Name: in.Name, Image: settings.WorkspaceImage, RepoURL: in.Repository.URL, Branch: in.Repository.Branch,
		CPU: in.Resources.CPU, Memory: in.Resources.Memory, Desired: desired, HostID: in.Placement.HostID, Disk: in.Resources.Disk}
	o, e := s.Store.Create(r.Context(), ws, key, fingerprint(metadata), ssh, credential)
	if e != nil {
		failure(w, r, e)
		return
	}
	data := operationReceipt(o)
	if o.HostID != "" {
		if reservation, err := s.Store.WorkspaceReservation(r.Context(), o.WorkspaceID); err == nil {
			data["reservation_status"] = reservation.Status
		}
	}
	w.Header().Set("Location", "/api/v1/operations/"+o.ID)
	reply(w, r, http.StatusAccepted, data)
}
func operationReceipt(o store.Operation) map[string]string {
	return map[string]string{"workspace_id": o.WorkspaceID, "assigned_host_id": o.HostID, "operation_id": o.ID, "status": o.Status}
}
func accepted(w http.ResponseWriter, r *http.Request, o store.Operation) {
	w.Header().Set("Location", "/api/v1/operations/"+o.ID)
	reply(w, r, 202, operationReceipt(o))
}
func (s *Server) operation(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Purge   bool   `json:"purge"`
			Confirm string `json:"confirm"`
		}
		if !parse(w, r, &in, true) {
			return
		}
		if in.Purge && kind != "delete" {
			bad(w, r, "Purge is only available when deleting a workspace.")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > 128 {
			bad(w, r, "Idempotency-Key is too long.")
			return
		}
		if (kind == "reset" || kind == "delete") && in.Confirm != workspaceID(r) {
			bad(w, r, "Confirm the workspace ID before resetting or deleting.")
			return
		}
		o, e := s.Store.Enqueue(r.Context(), workspaceID(r), kind, in.Purge, key, fingerprint([]any{workspaceID(r), kind, in.Purge}))
		if e != nil {
			failure(w, r, e)
			return
		}
		accepted(w, r, o)
	}
}
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	if _, e := s.Store.Workspace(r.Context(), workspaceID(r)); e != nil {
		failure(w, r, e)
		return
	}
	v, e := s.Store.Containers(r.Context(), workspaceID(r))
	send(w, r, v, e)
}
func (s *Server) inspectCurrent(ctx context.Context, r *http.Request) (store.Workspace, runtime.Container, error) {
	ws, e := s.Store.Workspace(ctx, workspaceID(r))
	if e != nil {
		return ws, runtime.Container{}, e
	}
	v, e := s.Inspect(ctx, ws)
	return ws, v, e
}
func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	ws, v, e := s.inspectCurrent(ctx, r)
	if d, ok := s.EngineFor(ws.HostID).(*runtime.Docker); ok && e == nil {
		d.InspectDigests(ctx, &v)
	}
	dockerSend(w, r, v, e)
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	ws, v, e := s.inspectCurrent(ctx, r)
	if e != nil {
		dockerFailure(w, r, e)
		return
	}
	if v.State != "running" {
		failure(w, r, store.ErrBusy)
		return
	}
	m, e := s.EngineFor(ws.HostID).Metrics(ctx, v.ID)
	if errors.Is(e, runtime.ErrMetricsNotReady) {
		e = store.ErrBusy
	}
	dockerSend(w, r, m, e)
}
func (s *Server) git(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	ws, v, e := s.inspectCurrent(ctx, r)
	if e != nil {
		dockerFailure(w, r, e)
		return
	}
	if v.State != "running" {
		failure(w, r, store.ErrBusy)
		return
	}
	g, e := control.Git(ctx, s.EngineFor(ws.HostID), v.ID)
	dockerSend(w, r, g, e)
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	ws, v, e := s.inspectCurrent(ctx, r)
	if e != nil {
		dockerFailure(w, r, e)
		return
	}
	logs, truncated, e := control.ReadOutput(func(w io.Writer) error { return s.EngineFor(ws.HostID).Logs(ctx, v.ID, w) })
	dockerSend(w, r, map[string]any{"text": logs, "truncated": truncated}, e)
}
func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Operations(r.Context(), workspaceID(r))
	send(w, r, v, e)
}
func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Operation(r.Context(), chi.URLParam(r, "operation"))
	send(w, r, v, e)
}
func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "operation")
	if _, e := s.Store.Operation(r.Context(), id); e != nil {
		failure(w, r, e)
		return
	}
	if e := s.Store.CancelOp(r.Context(), id); e != nil {
		failure(w, r, e)
		return
	}
	s.getOperation(w, r)
}
