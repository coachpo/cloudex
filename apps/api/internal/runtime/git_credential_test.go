package runtime

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestApplyGitCredentialToNewWorkspace(t *testing.T) {
	spec := Spec{WorkspaceID: "workspace", Generation: 1}
	credential := GitCredential{Host: "git.example.test:8443", Username: "fixture", Token: "synthetic-git-token"}
	const id = "fixture-container"
	const name = "/cloudex-fixture-inst-workspace-g1"
	for _, tc := range []struct {
		name       string
		state      container.ContainerState
		copyFailed bool
	}{
		{name: "archive", state: container.StateCreated},
		{name: "running", state: container.StateRunning},
		{name: "previously started", state: container.StateExited},
		{name: "copy error", state: container.StateCreated, copyFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v1.54/containers" + name + "/json":
					writeCodexTargetInspect(w, id, name, tc.state)
				case "PUT /v1.54/containers/" + id + "/archive":
					if tc.state != container.StateCreated || r.URL.Query().Get("path") != "/home/coder/.config/cloudex" || r.URL.Query().Has("copyUIDGID") {
						t.Errorf("unexpected credential copy request: %s", r.URL)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					archive := tar.NewReader(r.Body)
					header, err := archive.Next()
					if err != nil || header.Name != "git-credential.json" || header.Typeflag != tar.TypeReg ||
						header.Mode != 0600 || header.Uid != 10001 || header.Gid != 10001 {
						t.Errorf("unexpected credential header: %+v, %v", header, err)
					}
					var got GitCredential
					if err := json.NewDecoder(archive).Decode(&got); err != nil || got != credential {
						t.Error("archive did not contain the saved credential")
					}
					if _, err := archive.Next(); !errors.Is(err, io.EOF) {
						t.Errorf("expected a single archive entry: %v", err)
					}
					if tc.copyFailed {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"message": credential.Token})
					}
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			err := d.ApplyGitCredential(t.Context(), spec, id, credential)
			wantError := tc.state != container.StateCreated || tc.copyFailed
			if (err != nil) != wantError || (err != nil && strings.Contains(err.Error(), credential.Token)) {
				t.Errorf("credential write had unexpected success or exposed the token")
			}
		})
	}
}
