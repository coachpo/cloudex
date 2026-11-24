package agentlink

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	_ "modernc.org/sqlite"
)

type operationKey struct{}

type operation struct {
	id        string
	workspace string
}

// WithOperation attaches the durable control-plane operation to runtime calls.
// A replay carries the same identity across control-plane and Agent restarts.
func WithOperation(ctx context.Context, operationID, workspaceID string) context.Context {
	return context.WithValue(ctx, operationKey{}, operation{id: operationID, workspace: workspaceID})
}

type journal struct{ *sql.DB }

func openJournal(ctx context.Context, path string) (*journal, error) {
	if path == "" {
		return nil, nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS mutations (
		operation_id TEXT NOT NULL,
		fingerprint TEXT NOT NULL,
		completed INTEGER NOT NULL,
		PRIMARY KEY(operation_id, fingerprint)
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &journal{db}, nil
}

// A successful purge must not run again during replay after a new volume has
// been created. Other methods inspect Docker's current state on each replay.
func (j *journal) begin(ctx context.Context, req request) (string, bool, error) {
	if j == nil || req.OperationID == "" {
		return "", false, nil
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", false, err
	}
	hash := sha256.Sum256(data)
	key := hex.EncodeToString(hash[:])
	var completed bool
	err = j.QueryRowContext(ctx, `SELECT completed FROM mutations WHERE operation_id=? AND fingerprint=?`, req.OperationID, key).Scan(&completed)
	if err == nil {
		return key, completed, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	_, err = j.ExecContext(ctx, `INSERT INTO mutations VALUES(?,?,0)`, req.OperationID, key)
	return key, false, err
}

func (j *journal) complete(ctx context.Context, operationID, key string) error {
	if key == "" {
		return nil
	}
	_, err := j.ExecContext(ctx, `UPDATE mutations SET completed=1 WHERE operation_id=? AND fingerprint=?`, operationID, key)
	return err
}

type workspaceGate struct {
	busy chan struct{}
	refs int
}

func (a *executor) lock(ctx context.Context, key string) (func(), error) {
	a.mu.Lock()
	gate := a.gates[key]
	if gate == nil {
		gate = &workspaceGate{busy: make(chan struct{}, 1)}
		a.gates[key] = gate
	}
	gate.refs++
	a.mu.Unlock()
	release := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(a.gates, key)
		}
	}
	select {
	case gate.busy <- struct{}{}:
		return func() { <-gate.busy; release() }, nil
	case <-ctx.Done():
		release()
		return nil, unavailable(ctx.Err())
	}
}

func (a *executor) mutate(caller context.Context, req request) (any, error) {
	key := req.WorkspaceID
	if req.Spec != nil {
		if key != "" && key != req.Spec.WorkspaceID {
			return nil, runtime.ErrOwnership
		}
		key = req.Spec.WorkspaceID
	}
	if req.Spec == nil && req.Method != "purge" {
		if docker, ok := a.Engine.(interface {
			ManagedWorkspace(context.Context, string) (string, error)
		}); ok {
			id, err := docker.ManagedWorkspace(caller, req.ID)
			if err != nil {
				return nil, err
			}
			if key != "" && key != id {
				return nil, runtime.ErrOwnership
			}
			key = id
		}
	}
	if key == "" {
		key = req.ID
	}
	if key == "" || (req.Method == "purge" && req.ID != key) {
		return nil, runtime.ErrOwnership
	}
	unlock, err := a.lock(caller, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := a.mutations.Acquire(caller); err != nil {
		return nil, unavailable(err)
	}
	defer a.mutations.Release()
	if caller.Err() != nil {
		return nil, unavailable(caller.Err())
	}
	// A disappearing control connection does not revoke a Docker mutation
	// already in progress. Settle it locally before accepting a replay.
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	defer cancel()
	fingerprint, completed, err := a.journal.begin(ctx, req)
	if err != nil {
		return nil, unavailable(err)
	}
	if completed && req.Method == "purge" {
		return nil, nil
	}
	value, err := a.dockerMutation(ctx, req)
	if ctx.Err() != nil {
		return nil, unavailable(ctx.Err())
	}
	if err != nil {
		return value, err
	}
	if err = a.journal.complete(ctx, req.OperationID, fingerprint); err != nil {
		return nil, unavailable(err)
	}
	return value, nil
}

func (a *executor) dockerMutation(ctx context.Context, req request) (any, error) {
	switch req.Method {
	case "apply-git-credential":
		if req.Spec == nil || req.GitCredential == nil || req.ID == "" {
			return nil, errors.New("Workspace and Git credential are required")
		}
		writer, ok := a.Engine.(runtime.GitCredentialWriter)
		if !ok {
			return nil, errors.New("the Host Agent cannot apply Git credentials")
		}
		if err := writer.ApplyGitCredential(ctx, *req.Spec, req.ID, *req.GitCredential); err != nil {
			return nil, runtime.GitCredentialWriteError{Err: err}
		}
		return nil, nil
	case "apply-codex-config":
		if req.Spec == nil || req.CodexConfig == nil || req.ID == "" {
			return nil, errors.New("Workspace and Codex configuration are required")
		}
		writer, ok := a.Engine.(runtime.CodexConfigWriter)
		if !ok {
			return nil, errors.New("the Host Agent cannot apply Codex configuration")
		}
		return nil, writer.ApplyCodexConfig(ctx, *req.Spec, req.ID, *req.CodexConfig)
	case "ensure", "remove":
		if req.Spec == nil {
			return nil, errors.New("workspace specification required")
		}
		if req.Method == "remove" {
			return nil, a.Engine.Remove(ctx, *req.Spec)
		}
		c, err := a.Engine.Ensure(ctx, *req.Spec)
		return encodeContainer(c), err
	case "purge":
		return nil, a.Engine.Purge(ctx, req.ID, req.IncludeSSHHostKey)
	}
	if err := a.validateContainer(ctx, req.ID); err != nil {
		return nil, err
	}
	switch req.Method {
	case "start":
		return nil, a.Engine.Start(ctx, req.ID)
	case "stop":
		return nil, a.Engine.Stop(ctx, req.ID)
	default:
		return nil, errors.New("unknown runtime mutation")
	}
}
