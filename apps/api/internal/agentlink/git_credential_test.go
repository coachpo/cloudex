package agentlink

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
)

type gitCredentialRuntime struct {
	fakeRuntime
	apply func(runtime.Spec, string, runtime.GitCredential) error
}

func (d *gitCredentialRuntime) ApplyGitCredential(_ context.Context, spec runtime.Spec, id string, credential runtime.GitCredential) error {
	return d.apply(spec, id, credential)
}

func TestApplyGitCredentialRoundTripsAndJournalsFingerprint(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	const host = "host"
	const id = "fixture-container"
	const operation = "credential-operation"
	spec := runtime.Spec{WorkspaceID: "ws", Generation: 2}
	credential := runtime.GitCredential{Host: "git.example.test", Username: "fixture", Token: "synthetic-git-token"}
	var writes atomic.Int32
	docker := &gitCredentialRuntime{apply: func(got runtime.Spec, gotID string, c runtime.GitCredential) error {
		if got != spec || gotID != id || c != credential {
			return errors.New("credential write reached the wrong container or received incorrect data")
		}
		if writes.Add(1) == 1 {
			return errors.New(credential.Token)
		}
		return nil
	}}
	path := filepath.Join(t.TempDir(), "commands.sqlite")
	startTestAgent(t, server, host, Agent{Engine: docker, JournalPath: path})
	awaitHeartbeat(t, beats, host)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = WithOperation(ctx, operation, spec.WorkspaceID)
	engine := hub.Engine(host).(runtime.GitCredentialWriter)
	if err := engine.ApplyGitCredential(ctx, spec, id, credential); err == nil || strings.Contains(err.Error(), credential.Token) {
		t.Fatal("failed Agent write did not return a safe error")
	}
	if err := engine.ApplyGitCredential(ctx, spec, id, credential); err != nil || writes.Load() != 2 {
		t.Fatalf("retried credential write = %v, calls = %d", err, writes.Load())
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	req := request{Method: "apply-git-credential", Spec: &spec, ID: id, GitCredential: &credential, OperationID: operation, WorkspaceID: spec.WorkspaceID}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	var fingerprint string
	var completed bool
	err = db.QueryRowContext(ctx, "SELECT fingerprint,completed FROM mutations WHERE operation_id=?", operation).Scan(&fingerprint, &completed)
	if err != nil || !completed || fingerprint != hex.EncodeToString(hash[:]) {
		t.Fatalf("credential write did not leave a completed request fingerprint: %v", err)
	}
}
