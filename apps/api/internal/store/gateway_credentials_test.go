package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func bindRunning(t *testing.T, db *Store, name string, grant Grant) (Workspace, GatewayCredential) {
	t.Helper()
	ctx := t.Context()
	workspace := Workspace{ID: ID(), Name: name, Image: "fixture", Branch: "main", Desired: "running", CPU: 1000, Memory: 1024}
	queued, err := db.CreateWithGrant(ctx, workspace, "", "", nil, grant.ID, grant.GrantEpoch)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := db.ClaimOp(ctx)
	if err != nil || op.ID != queued.ID {
		t.Fatalf("claim create: %+v %v", op, err)
	}
	container := Container{Generation: op.Target, ID: ID(), ImageID: "fixture", Status: "running"}
	if err := db.FinishOp(ctx, op, "running", &container, ""); err != nil {
		t.Fatal(err)
	}
	workspace, err = db.Workspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred := GatewayCredential{
		WorkspaceID: workspace.ID, HostID: workspace.HostID, DockerID: container.ID, StartedAt: "fixture-first-start",
		Generation: workspace.Generation, GrantID: grant.ID, BindingRevision: 1, GrantEpoch: grant.GrantEpoch,
		Digest: GatewayCredentialDigest(ID()),
	}
	cred, err = db.IssueCredential(ctx, cred)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AuthenticateCredential(ctx, cred.Digest); !errors.Is(err, ErrMissing) {
		t.Fatalf("pending credential admitted: %v", err)
	}
	if err := db.ActivateCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	return workspace, cred
}

func TestSetBindingFencesPendingInstallationBothDirections(t *testing.T) {
	for _, channels := range [][2]string{{"codex", "chatgpt"}, {"chatgpt", "codex"}} {
		t.Run(channels[0]+"-to-"+channels[1], func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			a := grantFixture(t, db, channels[0], "registration-a", "upstream-a")
			w1, c1 := bindRunning(t, db, "shared-one", a)
			w2, c2 := bindRunning(t, db, "shared-two", a)
			b := grantFixture(t, db, channels[1], "registration-b", "upstream-b")
			if _, err := db.AuthenticateCredential(ctx, c1.Digest); err != nil {
				t.Fatalf("adding grant changed W1 admission: %v", err)
			}
			key := SubmissionKey{CredentialDigest: c1.Digest, RunID: "fixture-run", TurnID: "fixture-turn"}
			if _, err := db.CompareSubmission(ctx, key, 0, Submission{Model: "fixture-model", Status: "submitting"}); err != nil {
				t.Fatal(err)
			}
			same, err := db.SetBinding(ctx, w1.ID, a.ID, 1, a.GrantEpoch)
			if err != nil || same.BindingRevision != 1 {
				t.Fatalf("idempotent binding: %+v %v", same, err)
			}
			if _, err := db.Submission(ctx, key); err != nil {
				t.Fatalf("idempotent binding discarded in-flight state: %v", err)
			}
			pending := c1
			pending.Digest = GatewayCredentialDigest(ID())
			pending, err = db.IssueCredential(ctx, pending)
			if err != nil {
				t.Fatal(err)
			}
			rebound, err := db.SetBinding(ctx, w1.ID, b.ID, same.BindingRevision, b.GrantEpoch)
			if err != nil || rebound.BindingRevision != 2 || rebound.GrantID != b.ID {
				t.Fatalf("binding transition: %+v %v", rebound, err)
			}
			if err := db.ActivateCredential(ctx, pending); !errors.Is(err, ErrConflict) {
				t.Fatalf("late installation became active: %v", err)
			}
			if _, err := db.AuthenticateCredential(ctx, pending.Digest); !errors.Is(err, ErrMissing) {
				t.Fatalf("old binding admitted: %v", err)
			}
			if latest, err := db.SetBinding(ctx, w1.ID, b.ID, same.BindingRevision, b.GrantEpoch); err != nil || latest != rebound {
				t.Fatalf("repeated successful binding changed again: %+v %v", latest, err)
			}
			if actual, err := db.AuthenticateCredential(ctx, c2.Digest); err != nil || actual.GrantID != a.ID {
				t.Fatalf("shared authorization for W2 changed: %+v %v", actual, err)
			}
			if workspaces, err := db.GrantWorkspaces(ctx, a.ID); err != nil || len(workspaces) != 1 || workspaces[0].ID != w2.ID {
				t.Fatalf("affected workspaces still include W1: %+v %v", workspaces, err)
			}
			if latest, err := db.SetBinding(ctx, w1.ID, a.ID, 1, a.GrantEpoch); !errors.Is(err, ErrConflict) || latest != rebound {
				t.Fatalf("stale mutation: %+v %v", latest, err)
			}
			next := pending
			next.GrantID, next.GrantEpoch, next.BindingRevision, next.Digest = b.ID, b.GrantEpoch, 2, GatewayCredentialDigest(ID())
			next, err = db.IssueCredential(ctx, next)
			if err != nil || next.Sequence <= pending.Sequence {
				t.Fatalf("replacement sequence: %+v %v", next, err)
			}
			if err := db.ActivateCredential(ctx, next); err != nil {
				t.Fatal(err)
			}
			if actual, err := db.AuthenticateCredential(ctx, next.Digest); err != nil || actual.GrantID != b.ID {
				t.Fatalf("new binding use: %+v %v", actual, err)
			}
			if _, err := db.Submission(ctx, key); !errors.Is(err, ErrMissing) {
				t.Fatalf("old binding session remained: %v", err)
			}
		})
	}
}

func TestLifecycleRevokesBeforeRemoteOperationAndFencesRestart(t *testing.T) {
	for _, operation := range []string{"stop", "start", "rebuild", "reset", "delete"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			grant := grantFixture(t, db, "codex", "first-party", "workspace")
			workspace, cred := bindRunning(t, db, "lifecycle", grant)
			if err := db.ActivateCredential(ctx, cred); err != nil {
				t.Fatalf("repeated installation confirmation: %v", err)
			}
			queued, err := db.Enqueue(ctx, workspace.ID, operation, false, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.AuthenticateCredential(ctx, cred.Digest); !errors.Is(err, ErrMissing) {
				t.Fatalf("credential accepted after operation enqueue: %v", err)
			}
			latest, err := db.WorkspaceCredential(ctx, workspace.ID)
			if err != nil || latest.Status != CredentialRevoked || latest.Sequence <= cred.Sequence {
				t.Fatalf("revocation not durable: %+v %v", latest, err)
			}
			if _, err := db.IssueCredential(ctx, cred); !errors.Is(err, ErrBusy) {
				t.Fatalf("credential issued before operation completion: %v", err)
			}
			if err := db.ActivateCredential(ctx, cred); !errors.Is(err, ErrConflict) {
				t.Fatalf("late install acknowledged: %v", err)
			}
			if operation == "delete" {
				if err := db.FinishOp(ctx, queued, "deleted", nil, ""); err != nil {
					t.Fatal(err)
				}
				if err := db.DeleteGrant(ctx, grant.ID); err != nil {
					t.Fatalf("deleted workspace left a grant reference: %v", err)
				}
			}
		})
	}
}

func TestSubmissionFenceAndReferencesSurviveStoreRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "submissions.sqlite")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	grant := grantFixture(t, db, "chatgpt", "issued", "")
	_, active := bindRunning(t, db, "submissions", grant)
	_, other := bindRunning(t, db, "other-submissions", grant)
	key := SubmissionKey{CredentialDigest: active.Digest, RunID: "fixture-run", TurnID: "fixture-turn"}
	initial := Submission{Status: "submitting", Model: "fixture-model"}
	current, err := db.CompareSubmission(ctx, key, 0, initial)
	if err != nil || current.Sequence != 1 {
		t.Fatalf("initial admission: %+v %v", current, err)
	}
	modelChange := current
	modelChange.Model = "different-model"
	if _, err := db.CompareSubmission(ctx, key, 1, modelChange); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed model under submission scope: %v", err)
	}
	session, call := GatewayCredentialDigest("session"), GatewayCredentialDigest("call")
	if err := db.RecordSubmissionReferences(ctx, active.Digest, session, []string{call, call}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := db.Submission(ctx, key); err != nil || !reflect.DeepEqual(actual, current) {
		t.Fatalf("durable admission: %+v %v", actual, err)
	}
	if _, err := db.CompareSubmission(ctx, key, 0, initial); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate admission: %v", err)
	}
	completed := current
	completed.Status, completed.ResponseID, completed.CallIDs = "completed", GatewayCredentialDigest("response"), []string{call}
	if actual, err := db.CompareSubmission(ctx, key, 1, completed); err != nil || actual.Sequence != 2 {
		t.Fatalf("complete: %+v %v", actual, err)
	}
	if _, err := db.CompareSubmission(ctx, key, 1, completed); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update committed: %v", err)
	}
	if owned, err := db.SubmissionReferences(ctx, active.Digest, session, []string{call}); err != nil || !owned {
		t.Fatalf("matching session reference unknown: %v %v", owned, err)
	}
	for _, scope := range [][2]string{{other.Digest, session}, {active.Digest, GatewayCredentialDigest("other-session")}} {
		if owned, err := db.SubmissionReferences(ctx, scope[0], scope[1], []string{call}); err != nil || owned {
			t.Fatalf("different session accepted reference: %v %v", owned, err)
		}
	}
	if err := db.RevokeWorkspaceCredential(ctx, active.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CompareSubmission(ctx, key, 2, completed); !errors.Is(err, ErrMissing) {
		t.Fatalf("revoked credential updated submission: %v", err)
	}
	if _, err := db.SubmissionReferences(ctx, active.Digest, session, []string{call}); !errors.Is(err, ErrMissing) {
		t.Fatalf("revoked credential read a reference: %v", err)
	}
	rotated := active
	rotated.Digest = GatewayCredentialDigest(ID())
	rotated, err = db.IssueCredential(ctx, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateCredential(ctx, rotated); err != nil {
		t.Fatal(err)
	}
	if owned, err := db.SubmissionReferences(ctx, rotated.Digest, session, []string{call}); err != nil || !owned {
		t.Fatalf("plain credential rotation lost same-instance reference: %v %v", owned, err)
	}
	restarted := rotated
	restarted.Digest, restarted.StartedAt = GatewayCredentialDigest(ID()), "fixture-second-start"
	restarted, err = db.IssueCredential(ctx, restarted)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateCredential(ctx, restarted); err != nil {
		t.Fatal(err)
	}
	if owned, err := db.SubmissionReferences(ctx, restarted.Digest, session, []string{call}); err != nil || owned {
		t.Fatalf("new Docker startup accepted the prior instance's reference: %v %v", owned, err)
	}
}
