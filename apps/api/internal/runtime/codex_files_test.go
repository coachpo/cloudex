package runtime

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestWriteInitialCodexFilesArchivesFilesOnNewContainer(t *testing.T) {
	const targetID = "workspace-container"
	spec := Spec{WorkspaceID: "workspace", Generation: 1}
	for _, tc := range []struct {
		name  string
		files InitialCodexFiles
	}{
		{"both files", InitialCodexFiles{AgentsMD: "# Project guidance\n", ConfigTOML: CodexConfigSeed}},
		{"instructions only", InitialCodexFiles{AgentsMD: "# Project guidance\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := tc.files
			var writes atomic.Int32
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v1.54/containers/cloudex-fixture-inst-workspace-g1/json":
					writeWorkspaceInspect(w, targetID, "/cloudex-fixture-inst-workspace-g1", container.StateCreated)
				case "PUT /v1.54/containers/" + targetID + "/archive":
					writes.Add(1)
					query := r.URL.Query()
					if query.Get("path") != "/home/coder/.codex" || query.Has("copyUIDGID") || query.Get("noOverwriteDirNonDir") != "true" {
						t.Errorf("unexpected Docker archive options: %s", query.Encode())
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					checkCodexArchive(t, r.Body, config)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			if err := d.WriteInitialCodexFiles(t.Context(), spec, targetID, config); err != nil || writes.Load() != 1 {
				t.Fatalf("config writes = %d, error = %v", writes.Load(), err)
			}
		})
	}
}

func checkCodexArchive(t *testing.T, archive io.Reader, config InitialCodexFiles) {
	t.Helper()
	type file struct {
		content string
		mode    int64
	}
	want := map[string]file{
		"AGENTS.md": {config.AgentsMD, 0644}, "config.toml": {config.ConfigTOML, 0600},
	}
	if config.ConfigTOML == "" {
		delete(want, "config.toml")
	}
	if config.AgentsMD == "" {
		delete(want, "AGENTS.md")
	}
	r := tar.NewReader(archive)
	for {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Errorf("read archive header: %v", err)
			return
		}
		expected, ok := want[header.Name]
		if !ok || header.Typeflag != tar.TypeReg || header.Uid != 10001 || header.Gid != 10001 || header.Mode != expected.mode {
			t.Errorf("unexpected archive header: %+v", header)
		}
		content, err := io.ReadAll(r)
		if err != nil || string(content) != expected.content {
			t.Errorf("unexpected contents for %s, error = %v", header.Name, err)
		}
		delete(want, header.Name)
	}
	if len(want) != 0 {
		t.Error("archive omitted initial files")
	}
}

func TestWriteInitialCodexFilesRejectsWrongOrPreviouslyStartedContainer(t *testing.T) {
	spec := Spec{WorkspaceID: "workspace", Generation: 1}
	const id = "workspace-container"
	const name = "/cloudex-fixture-inst-workspace-g1"
	for _, tc := range []struct {
		name, id, containerName string
		state                   container.ContainerState
		wantOwnershipError      bool
	}{
		{"wrong ID", "another-container", name, container.StateCreated, true},
		{"wrong name", id, "/another-container", container.StateCreated, true},
		{"running", id, name, container.StateRunning, false},
		{"previously started", id, name, container.StateExited, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1.54/containers"+name+"/json" {
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				writeWorkspaceInspect(w, tc.id, tc.containerName, tc.state)
			})
			if err := d.WriteInitialCodexFiles(t.Context(), spec, id, InitialCodexFiles{}); err == nil || errors.Is(err, ErrOwnership) != tc.wantOwnershipError {
				t.Fatalf("unexpected config write result: %v", err)
			}
		})
	}
}

func writeWorkspaceInspect(w http.ResponseWriter, id, name string, state container.ContainerState) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(container.InspectResponse{
		ID: id, Name: name, State: &container.State{Status: state, Running: state == container.StateRunning},
		Config: &container.Config{User: "0:0", Labels: map[string]string{
			instanceLabel: "fixture-instance", "cloud.managed": "true", hostLabel: "",
			workspaceLabel: "workspace", generationLabel: "1",
		}},
	})
}
