package httpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const deletionSession = "host-deletion-test-session"

type deletionSSHConnections struct {
	sshHostConnections
	removed func(string)
}

func (c deletionSSHConnections) Removed(id string) { c.removed(id) }

func deletionAPI(t *testing.T, db *store.Store, disconnectAgent, removeSSH func(string)) http.Handler {
	t.Helper()
	if err := db.CreateAdmin(t.Context(), "admin", syntheticPassword, digest(deletionSession), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return (&Server{
		Control:         &control.Control{Store: db},
		Origin:          testOrigin,
		DisconnectAgent: disconnectAgent,
		SSHHosts:        deletionSSHConnections{removed: removeSSH},
	}).Handler(http.NotFoundHandler())
}

func deletionRequest(handler http.Handler, method, path, body, origin string, authenticated bool) *httptest.ResponseRecorder {
	return deletionRequestWithKey(handler, method, path, body, origin, authenticated, "")
}

func deletionRequestWithKey(handler http.Handler, method, path, body, origin string, authenticated bool, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	req.Header.Set("Origin", origin)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: deletionSession})
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func expectDeletionResponse(t *testing.T, res *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", res.Code, status, res.Body)
	}
	if code == "" {
		return
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil || body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("problem %q, want %s with a message: %v", res.Body, code, err)
	}
}

func hostDeletionData[T any](t *testing.T, res *httptest.ResponseRecorder) T {
	t.Helper()
	expectDeletionResponse(t, res, http.StatusOK, "")
	var body struct{ Data T }
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func addDeletionAgent(t *testing.T, db *store.Store, state string) (id, token, serial string) {
	t.Helper()
	ctx := t.Context()
	token, serial = store.ID(), store.ID()
	host, err := db.AddHost(ctx, "Remote "+token, token, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if state == "enrolling" {
		return host.ID, token, serial
	}
	if err := db.EnrollHost(ctx, host.ID, token, serial); err != nil {
		t.Fatal(err)
	}
	token = store.ID()
	if _, err := db.RenewEnrollment(ctx, host.ID, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if state != "offline" {
		if err := db.Heartbeat(ctx, host.ID, serial, store.HostReport{
			Healthy: state == "online", Images: []store.ImageStatus{{Image: store.DefaultWorkspaceImage, Ready: true}},
			MaxOperations: 2, CPU: 4000, Memory: 8 << 30,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SetHostDisk(ctx, host.ID, 1<<30); err != nil {
		t.Fatal(err)
	}
	return host.ID, token, serial
}

func TestDeleteHostAccessAndDisconnect(t *testing.T) {
	db, ctx := newDB(t), t.Context()
	var disconnected, removed []string
	record := func(dst *[]string) func(string) {
		return func(id string) {
			if _, err := db.Host(ctx, id); !errors.Is(err, store.ErrMissing) {
				t.Errorf("disconnect called before registration disappeared: %v", err)
			}
			*dst = append(*dst, id)
		}
	}
	h := deletionAPI(t, db, record(&disconnected), record(&removed))
	del := func(path, origin string, authenticated bool) *httptest.ResponseRecorder {
		return deletionRequest(h, http.MethodDelete, path, "", origin, authenticated)
	}
	agent, token, serial := addDeletionAgent(t, db, "online")
	path := "/hosts/" + agent
	expectDeletionResponse(t, del(path, testOrigin, false), http.StatusUnauthorized, "UNAUTHORIZED")
	expectDeletionResponse(t, del(path, "https://other.example", true), http.StatusForbidden, "ORIGIN_REJECTED")
	expectDeletionResponse(t, del("/hosts/local", testOrigin, true), http.StatusConflict, "HOST_LOCAL_PROTECTED")
	expectDeletionResponse(t, del("/hosts/missing", testOrigin, true), http.StatusNotFound, "NOT_FOUND")
	if len(disconnected)+len(removed) != 0 {
		t.Fatalf("denied deletion disconnected a Host: %v, %v", disconnected, removed)
	}
	if err := db.AuthenticateHost(ctx, agent, serial); err != nil {
		t.Fatal(err)
	}
	expectDeletionResponse(t, del(path, testOrigin, true), http.StatusNoContent, "")
	expectDeletionResponse(t, del(path, testOrigin, true), http.StatusNotFound, "NOT_FOUND")
	if !reflect.DeepEqual(disconnected, []string{agent}) {
		t.Fatalf("Agent disconnects: got %v, want [%s]", disconnected, agent)
	}
	if _, err := db.PendingHost(ctx, token); !errors.Is(err, store.ErrInvalidEnrollment) {
		t.Fatalf("pending Agent token survived: %v", err)
	}
	if err := db.AuthenticateHost(ctx, agent, serial); !errors.Is(err, store.ErrHostUnauthorized) {
		t.Fatalf("old Agent identity survived: %v", err)
	}
	if err := db.Heartbeat(ctx, agent, serial, store.HostReport{Healthy: true, MaxOperations: 1}); !errors.Is(err, store.ErrHostUnauthorized) {
		t.Fatalf("late Agent heartbeat accepted: %v", err)
	}

	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ssh, err := db.AddSSHHost(ctx, "Remote SSH", hostssh.Config{Address: "ssh.example:22", User: "fixture", Password: "fixture"}, key)
	if err != nil {
		t.Fatal(err)
	}
	report := store.HostReport{Healthy: true, MaxOperations: 1}
	if err := db.SSHHeartbeat(ctx, ssh.ID, ssh.SSHRevision, report); err != nil {
		t.Fatal(err)
	}
	sshPath := "/hosts/" + ssh.ID
	expectDeletionResponse(t, del(sshPath, testOrigin, true), http.StatusNoContent, "")
	if !reflect.DeepEqual(removed, []string{ssh.ID}) {
		t.Fatalf("SSH removals: got %v, want [%s]", removed, ssh.ID)
	}
	if _, err := db.SSHConfig(ctx, ssh.ID, key); !errors.Is(err, store.ErrMissing) {
		t.Fatalf("old SSH configuration still available: %v", err)
	}
	if err := db.SSHHeartbeat(ctx, ssh.ID, ssh.SSHRevision, report); !errors.Is(err, store.ErrHostUnauthorized) {
		t.Fatalf("late SSH heartbeat accepted: %v", err)
	}
	expectDeletionResponse(t, deletionRequest(h, http.MethodPost, sshPath+"/ssh/reconnect", "", testOrigin, true), http.StatusNotFound, "NOT_FOUND")
	for _, deletedPath := range []string{path, sshPath} {
		expectDeletionResponse(t, deletionRequest(h, http.MethodGet, deletedPath, "", testOrigin, true), http.StatusNotFound, "NOT_FOUND")
	}
	hosts := hostDeletionData[[]store.Host](t, deletionRequest(h, http.MethodGet, "/hosts", "", testOrigin, true))
	if len(hosts) != 1 || hosts[0].ID != store.LocalHostID {
		t.Fatalf("Hosts list after deletions: %+v", hosts)
	}
}

func TestDeleteHostIgnoresConnectionAndSchedulingState(t *testing.T) {
	db := newDB(t)
	h := deletionAPI(t, db, nil, nil)
	for _, fixture := range []struct{ state, mode string }{
		{"enrolling", "enabled"}, {"offline", "enabled"}, {"degraded", "enabled"},
		{"online", "cordoned"}, {"offline", "disabled"},
	} {
		t.Run(fixture.state+"/"+fixture.mode, func(t *testing.T) {
			id, _, _ := addDeletionAgent(t, db, fixture.state)
			host, err := db.SetHostScheduling(t.Context(), id, fixture.mode)
			if err != nil || host.Status != fixture.state || host.Scheduling != fixture.mode {
				t.Fatalf("Host fixture: %+v, %v", host, err)
			}
			expectDeletionResponse(t, deletionRequest(h, http.MethodDelete, "/hosts/"+id, "", testOrigin, true), http.StatusNoContent, "")
		})
	}
}

func TestDeleteHostRejectsDependencies(t *testing.T) {
	for _, state := range []string{"running workspace", "stopped workspace", "error workspace", "queued", "running", "requeued"} {
		t.Run(state, func(t *testing.T) {
			db, ctx := newDB(t), t.Context()
			h := deletionAPI(t, db, func(id string) { t.Errorf("denied deletion disconnected %s", id) }, nil)
			id, token, serial := addDeletionAgent(t, db, "online")
			w := store.Workspace{ID: store.ID(), Name: "Fixture", HostID: id, Image: store.DefaultWorkspaceImage,
				Branch: "main", CPU: 500, Memory: 512, Disk: 1024, Desired: "running"}
			op, err := db.Create(ctx, w, "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			code := "HOST_HAS_PENDING_OPERATIONS"
			if observed, ok := strings.CutSuffix(state, " workspace"); ok {
				code = "HOST_HAS_WORKSPACES"
				message := ""
				if observed == "error" {
					message = "fixture creation failed"
				}
				if err := db.FinishOp(ctx, op, observed, nil, message); err != nil {
					t.Fatal(err)
				}
			} else if state != "queued" {
				op, _, err = db.ClaimOp(ctx, true)
				if err != nil {
					t.Fatal(err)
				}
				if state == "requeued" {
					if err := db.RetryOp(ctx, op.ID, "operation outcome unknown"); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := hostDeletionData[store.Host](t, deletionRequest(h, http.MethodGet, "/hosts/"+id, "", testOrigin, true))
			originalOp := hostDeletionData[store.Operation](t, deletionRequest(h, http.MethodGet, "/operations/"+op.ID, "", testOrigin, true))
			path := "/hosts/" + id
			originalWorkspace, err := db.Workspace(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			originalReservations := hostDeletionData[[]store.Reservation](t, deletionRequest(h, http.MethodGet, path+"/reservations", "", testOrigin, true))
			expectDeletionResponse(t, deletionRequest(h, http.MethodDelete, path, "", testOrigin, true), http.StatusConflict, code)
			after := hostDeletionData[store.Host](t, deletionRequest(h, http.MethodGet, path, "", testOrigin, true))
			afterOp := hostDeletionData[store.Operation](t, deletionRequest(h, http.MethodGet, "/operations/"+op.ID, "", testOrigin, true))
			if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(afterOp, originalOp) {
				t.Fatalf("rejection changed Host or operation: %+v, %+v", after, afterOp)
			}
			if afterWorkspace, err := db.Workspace(ctx, w.ID); err != nil || !reflect.DeepEqual(afterWorkspace, originalWorkspace) {
				t.Fatalf("rejection changed Workspace: %+v, %v", afterWorkspace, err)
			}
			if _, err := db.PendingHost(ctx, token); err != nil {
				t.Fatalf("rejection invalidated the pending token: %v", err)
			}
			if err := db.AuthenticateHost(ctx, id, serial); err != nil {
				t.Fatalf("rejection invalidated the Agent identity: %v", err)
			}
			reservations := hostDeletionData[[]store.Reservation](t, deletionRequest(h, http.MethodGet, path+"/reservations", "", testOrigin, true))
			if len(reservations) != 1 || reservations[0].WorkspaceID != w.ID || !reflect.DeepEqual(reservations, originalReservations) {
				t.Fatalf("rejection changed the reservation: %+v", reservations)
			}
		})
	}
}

func TestDeleteHostClearsWorkspaceHistory(t *testing.T) {
	db, ctx := newDB(t), t.Context()
	h := deletionAPI(t, db, nil, nil)
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name, reservation, replacement string
		purge                          bool
		disk                           int64
	}{
		{"purged", "released", "agent", true, 1024},
		{"retained", "retained", "ssh", false, 1024},
		{"retained with zero reservation", "released", "agent", false, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			id, _, _ := addDeletionAgent(t, db, "online")
			original, err := db.Host(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			register := func(name string) (store.Host, error) {
				if fixture.replacement == "ssh" {
					return db.AddSSHHost(ctx, name, hostssh.Config{Address: "ssh.example:22", User: "fixture", Password: "fixture"}, key)
				}
				return db.AddHost(ctx, name, store.ID(), time.Now().Add(time.Hour))
			}
			if _, err := register(strings.ToUpper(original.Name)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name on registered Host: %v", err)
			}
			w := store.Workspace{ID: store.ID(), Name: fixture.name, HostID: id, Image: store.DefaultWorkspaceImage,
				Branch: "main", CPU: 500, Memory: 512, Disk: fixture.disk, Desired: "running"}
			creation, err := db.Create(ctx, w, "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			c := &store.Container{ID: store.ID(), Generation: 1, ImageID: "fixture", Status: "running"}
			if err := db.FinishOp(ctx, creation, "running", c, ""); err != nil {
				t.Fatal(err)
			}
			workspacePath := "/workspaces/" + w.ID
			body := fmt.Sprintf(`{"confirm":%q,"purge":%t}`, w.ID, fixture.purge)
			deleteWorkspace := func() *httptest.ResponseRecorder {
				return deletionRequestWithKey(h, http.MethodDelete, workspacePath, body, testOrigin, true, "delete-"+w.ID)
			}
			first := deleteWorkspace()
			expectDeletionResponse(t, first, http.StatusAccepted, "")
			location := first.Header().Get("Location")
			deletion, err := db.Operation(ctx, strings.TrimPrefix(location, "/api/v1/operations/"))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.FinishOp(ctx, deletion, "deleted", nil, ""); err != nil {
				t.Fatal(err)
			}
			reservation, err := db.WorkspaceReservation(ctx, w.ID)
			if err != nil || reservation.Status != fixture.reservation {
				t.Fatalf("reservation before deletion: %+v, %v", reservation, err)
			}
			replayed := deleteWorkspace()
			expectDeletionResponse(t, replayed, http.StatusAccepted, "")
			if replayed.Header().Get("Location") != location {
				t.Fatalf("Workspace DELETE replay returned another operation: %q", replayed.Header().Get("Location"))
			}
			historyPath := workspacePath + "/operations"
			history := hostDeletionData[[]store.Operation](t, deletionRequest(h, http.MethodGet, historyPath, "", testOrigin, true))
			if len(history) != 2 {
				t.Fatalf("expected create and delete history before deleting Host: %+v", history)
			}
			expectDeletionResponse(t, deletionRequest(h, http.MethodDelete, "/hosts/"+id, "", testOrigin, true), http.StatusNoContent, "")
			replacement, err := register(original.Name)
			if err != nil {
				t.Fatalf("register %s with the removed Host's name: %v", fixture.replacement, err)
			}
			if replacement.ID == "" || replacement.ID == id || replacement.Name != original.Name || replacement.ConnectionType != fixture.replacement {
				t.Fatalf("unexpected replacement Host: %+v", replacement)
			}
			expectDeletionResponse(t, deletionRequest(h, http.MethodGet, "/hosts/"+id, "", testOrigin, true), http.StatusNotFound, "NOT_FOUND")
			newHistory := hostDeletionData[[]store.Operation](t, deletionRequest(h, http.MethodGet, "/hosts/"+replacement.ID+"/operations", "", testOrigin, true))
			if len(newHistory) != 0 {
				t.Fatalf("replacement Host inherited operations: %+v", newHistory)
			}
			after := hostDeletionData[[]store.Operation](t, deletionRequest(h, http.MethodGet, historyPath, "", testOrigin, true))
			if len(after) != 0 {
				t.Fatalf("Workspace history survived Host removal: %+v", after)
			}
			for _, op := range history {
				expectDeletionResponse(t, deletionRequest(h, http.MethodGet, "/operations/"+op.ID, "", testOrigin, true), http.StatusNotFound, "NOT_FOUND")
			}
			expectDeletionResponse(t, deleteWorkspace(), http.StatusNotFound, "NOT_FOUND")
			if _, err := db.WorkspaceReservation(ctx, w.ID); !errors.Is(err, store.ErrMissing) {
				t.Fatalf("removed Workspace reservation lookup: %v", err)
			}
		})
	}
}

func TestDeleteHostConcurrentRequests(t *testing.T) {
	db := newDB(t)
	id, _, _ := addDeletionAgent(t, db, "online")
	h := deletionAPI(t, db, nil, nil)
	const count = 8
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, count)
	for range count {
		go func() {
			<-start
			results <- deletionRequest(h, http.MethodDelete, "/hosts/"+id, "", testOrigin, true)
		}()
	}
	close(start)
	successes := 0
	for range count {
		res := <-results
		if res.Code == http.StatusNoContent {
			successes++
		} else {
			expectDeletionResponse(t, res, http.StatusNotFound, "NOT_FOUND")
		}
	}
	if successes != 1 {
		t.Fatalf("successful requests: %d, want 1", successes)
	}
}

func TestDeleteHostConcurrentPlacement(t *testing.T) {
	db := newDB(t)
	h := deletionAPI(t, db, nil, nil)
	for _, order := range []string{"placement first", "deletion first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			id, _, _ := addDeletionAgent(t, db, "online")
			hostPath := "/hosts/" + id
			body := fmt.Sprintf(`{"name":%q,"placement":{"type":"manual","host_id":%q}}`, order, id)
			place := func() *httptest.ResponseRecorder {
				return deletionRequest(h, http.MethodPost, "/workspaces", body, testOrigin, true)
			}
			remove := func() *httptest.ResponseRecorder {
				return deletionRequest(h, http.MethodDelete, hostPath, "", testOrigin, true)
			}
			var createRes, deleteRes *httptest.ResponseRecorder
			switch order {
			case "placement first":
				createRes, deleteRes = place(), remove()
				expectDeletionResponse(t, createRes, http.StatusAccepted, "")
			case "deletion first":
				deleteRes, createRes = remove(), place()
				expectDeletionResponse(t, deleteRes, http.StatusNoContent, "")
			default:
				start := make(chan struct{})
				created := make(chan *httptest.ResponseRecorder, 1)
				deleted := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					<-start
					created <- place()
				}()
				go func() {
					<-start
					deleted <- remove()
				}()
				close(start)
				createRes, deleteRes = <-created, <-deleted
			}
			if createRes.Code == http.StatusAccepted {
				expectDeletionResponse(t, deleteRes, http.StatusConflict, "HOST_HAS_PENDING_OPERATIONS")
				hostDeletionData[store.Host](t, deletionRequest(h, http.MethodGet, hostPath, "", testOrigin, true))
				reservations := hostDeletionData[[]store.Reservation](t, deletionRequest(h, http.MethodGet, hostPath+"/reservations", "", testOrigin, true))
				operations := hostDeletionData[[]store.Operation](t, deletionRequest(h, http.MethodGet, hostPath+"/operations", "", testOrigin, true))
				if len(reservations) != 1 || len(operations) != 1 || operations[0].Status != "queued" || operations[0].WorkspaceID != reservations[0].WorkspaceID {
					t.Fatalf("successful placement state: operations=%+v reservations=%+v", operations, reservations)
				}
			} else {
				expectDeletionResponse(t, deleteRes, http.StatusNoContent, "")
				expectDeletionResponse(t, createRes, http.StatusNotFound, "NOT_FOUND")
				reservations, err := db.HostReservations(t.Context(), id)
				if err != nil || len(reservations) != 0 {
					t.Fatalf("deleted Host has orphaned reservations: %+v, %v", reservations, err)
				}
				operations, err := db.HostOperations(t.Context(), id)
				if err != nil || len(operations) != 0 {
					t.Fatalf("deleted Host has orphaned operations: %+v, %v", operations, err)
				}
			}
		})
	}
}

func TestDeleteHostStorageFailureRollsBack(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, token, serial := addDeletionAgent(t, db, "online")
	workspace := store.Workspace{ID: store.ID(), Name: "Fixture", HostID: id, Image: store.DefaultWorkspaceImage,
		Branch: "main", CPU: 500, Memory: 512, Disk: 1024, Desired: "running"}
	op, err := db.Create(ctx, workspace, "rollback-create", "hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CancelOp(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	h := deletionAPI(t, db, func(id string) { t.Errorf("failed deletion disconnected %s", id) }, nil)
	path := "/hosts/" + id
	before := hostDeletionData[store.Host](t, deletionRequest(h, http.MethodGet, path, "", testOrigin, true))
	operationPath := "/operations/" + op.ID
	beforeOp := hostDeletionData[store.Operation](t, deletionRequest(h, http.MethodGet, operationPath, "", testOrigin, true))
	beforeReservation, err := db.WorkspaceReservation(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	fault, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fault.Close() })
	// FAIL preserves completed statement changes until the Store rolls back the transaction.
	_, err = fault.ExecContext(ctx, `CREATE TRIGGER reject_host_removal AFTER DELETE ON host_nodes
		BEGIN SELECT RAISE(FAIL, 'test Host cascade failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	expectDeletionResponse(t, deletionRequest(h, http.MethodDelete, path, "", testOrigin, true), http.StatusInternalServerError, "INTERNAL")
	after := hostDeletionData[store.Host](t, deletionRequest(h, http.MethodGet, path, "", testOrigin, true))
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed deletion changed Host: before=%+v after=%+v", before, after)
	}
	afterOp := hostDeletionData[store.Operation](t, deletionRequest(h, http.MethodGet, operationPath, "", testOrigin, true))
	if !reflect.DeepEqual(afterOp, beforeOp) {
		t.Fatalf("failed deletion changed operation: before=%+v after=%+v", beforeOp, afterOp)
	}
	afterReservation, err := db.WorkspaceReservation(ctx, workspace.ID)
	if err != nil || !reflect.DeepEqual(afterReservation, beforeReservation) {
		t.Fatalf("failed deletion changed reservation: before=%+v after=%+v err=%v", beforeReservation, afterReservation, err)
	}
	if replay, err := db.IdempotentOperation(ctx, "rollback-create", "hash"); err != nil || replay.ID != op.ID {
		t.Fatalf("failed deletion removed idempotency replay: op=%+v err=%v", replay, err)
	}
	if _, err := db.PendingHost(ctx, token); err != nil {
		t.Fatalf("failed deletion invalidated token: %v", err)
	}
	if err := db.AuthenticateHost(ctx, id, serial); err != nil {
		t.Fatalf("failed deletion invalidated Agent identity: %v", err)
	}
}
