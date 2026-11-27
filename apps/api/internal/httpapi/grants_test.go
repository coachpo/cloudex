package httpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/codexgateway"
	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestGrantManagementRequiresAdminAndBindingRevision(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "grants.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	service, err := codexgateway.New(ctx, codexgateway.Config{Store: db, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	a := newTestServer(t, &Server{Control: &control.Control{Store: db, Key: key}, Origin: testOrigin, GrantGateway: service})
	a.call("GET", "/grants", nil, http.StatusUnauthorized, nil)
	a.call("POST", "/auth/setup", credentials{Username: "admin", Password: syntheticPassword}, http.StatusOK, nil)

	grant := store.Grant{ID: store.ID(), Channel: "chatgpt", ClientID: "issued-test", Issuer: "https://issuer.example.test",
		Subject: "test-subject", Status: store.GrantActive, Credential: []byte("synthetic-ciphertext"), ExpiresAt: time.Now().Add(time.Hour)}
	grant.RegistrationID = store.GrantRegistrationID(grant.Channel, grant.Issuer, grant.ClientID)
	if err := db.AddGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	workspace := store.Workspace{ID: store.ID(), Name: "bound-test", Image: "test", Branch: "main", Desired: "stopped", CPU: 1000, Memory: 1024}
	if _, err := db.CreateWithGrant(ctx, workspace, "", "", nil, grant.ID, 1); err != nil {
		t.Fatal(err)
	}
	var listed struct{ Grants []grantView }
	a.call("GET", "/grants", nil, http.StatusOK, &listed)
	if len(listed.Grants) != 1 || len(listed.Grants[0].WorkspaceIDs) != 1 || listed.Grants[0].WorkspaceIDs[0] != workspace.ID ||
		listed.Grants[0].Subject != grant.Subject || listed.Grants[0].Credential != nil {
		t.Fatal("grant list did not identify its affected Workspace or exposed credentials")
	}
	a.call("DELETE", "/grants/"+grant.ID, nil, http.StatusConflict, nil)
	path := "/workspaces/" + workspace.ID + "/grant-binding"
	a.call("DELETE", path, nil, http.StatusPreconditionRequired, nil)
	request := func(origin string, revision int64, wantStatus int, wantCode string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.server.URL+"/api/v1"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(revision, 10)))
		res, err := a.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != wantStatus {
			t.Fatalf("unbind HTTP %d, want %d: %s (%v)", res.StatusCode, wantStatus, body, err)
		}
		if wantCode != "" {
			var problem struct{ Error struct{ Code string } }
			if err := json.Unmarshal(body, &problem); err != nil || problem.Error.Code != wantCode {
				t.Fatalf("unexpected problem code %q: %v", problem.Error.Code, err)
			}
		}
		return res.Header.Get("ETag")
	}
	request("https://other.example.test", 1, http.StatusForbidden, "ORIGIN_REJECTED")
	request(testOrigin, 0, http.StatusConflict, "binding_conflict")
	for range 2 {
		if tag := request(testOrigin, 1, http.StatusOK, ""); tag != "\"2\"" {
			t.Fatalf("unbind or replay returned revision %s, want 2", tag)
		}
	}
	a.call("DELETE", "/grants/"+grant.ID, nil, http.StatusNoContent, nil)
}
