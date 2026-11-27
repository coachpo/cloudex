package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestDeleteHostCascadesByWorkspaceOwnership(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	capacity := Resources{CPU: 4000, Memory: 4 * gib, Disk: 4 * gib}
	target := connectedHost(t, s, "target", capacity)
	other := connectedHost(t, s, "other", capacity)
	grant := grantFixture(t, s, "chatgpt", "shared", "")
	const image = "global:fixture"
	if err := s.SetWorkspaceImage(ctx, image); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	type fixture struct {
		hostID, workspaceID, operationID, digest string
	}
	seed := func(host, historicalHost Host) fixture {
		t.Helper()
		if _, err := s.RenewEnrollment(ctx, host.ID, host.Name+"-pending", time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		w := hostWorkspace(host, host.Name+" Workspace")
		repo := RepositoryCredential{Host: "git.example.test", Username: "fixture", Token: "fixture-token"}
		op, err := s.CreateWithGrant(ctx, w, host.Name+"-creation", "fixture-hash", &repo, grant.ID, grant.GrantEpoch)
		if err != nil {
			t.Fatal(err)
		}
		c := Container{ID: ID(), Generation: op.Target, ImageID: "workspace", Status: "running"}
		if err := s.FinishOp(ctx, op, "running", &c, ""); err != nil {
			t.Fatal(err)
		}
		cred, err := s.IssueCredential(ctx, GatewayCredential{
			WorkspaceID: w.ID, HostID: host.ID, DockerID: c.ID, StartedAt: "fixture-start",
			Generation: c.Generation, GrantID: grant.ID, BindingRevision: 1, GrantEpoch: grant.GrantEpoch,
			Digest: GatewayCredentialDigest(ID()),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ActivateCredential(ctx, cred); err != nil {
			t.Fatal(err)
		}
		key := SubmissionKey{CredentialDigest: cred.Digest, RunID: "run", TurnID: "turn"}
		if _, err := s.CompareSubmission(ctx, key, 0, Submission{Model: "fixture", Status: "submitting"}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordSubmissionReferences(ctx, cred.Digest, GatewayCredentialDigest("session"), []string{GatewayCredentialDigest("reference")}); err != nil {
			t.Fatal(err)
		}
		// Seed residual Workspace credentials and divergent routing metadata to
		// verify the Host cascade follows assignment, independently of lifecycle cleanup.
		exec(`UPDATE workspaces SET deleted_at=?,observed_state='deleted' WHERE id=?`, now(), w.ID)
		for _, table := range []string{"operations", "containers", "workspace_reservations", "gateway_credentials"} {
			exec(`UPDATE `+table+` SET host_id=? WHERE workspace_id=?`, historicalHost.ID, w.ID)
		}
		return fixture{host.ID, w.ID, op.ID, cred.Digest}
	}
	removed, kept := seed(target, other), seed(other, target)
	_, liveCredential := bindRunning(t, s, "Live Workspace with shared grant", grant)
	liveBefore, err := s.AuthenticateCredential(ctx, liveCredential.Digest)
	if err != nil {
		t.Fatal(err)
	}
	assertRecords := func(f fixture, want int) {
		t.Helper()
		for _, row := range []struct {
			table, key, id string
			count          int
		}{
			{"host_nodes", "id", f.hostID, want},
			{"host_enrollment_tokens", "host_id", f.hostID, 2 * want},
			{"host_capacity", "host_id", f.hostID, want},
			{"host_images", "host_id", f.hostID, want},
			{"workspaces", "id", f.workspaceID, want},
			{"operations", "workspace_id", f.workspaceID, want},
			{"containers", "workspace_id", f.workspaceID, want},
			{"workspace_reservations", "workspace_id", f.workspaceID, want},
			{"workspace_repository_credentials", "workspace_id", f.workspaceID, want},
			{"workspace_grant_bindings", "workspace_id", f.workspaceID, want},
			{"gateway_credentials", "workspace_id", f.workspaceID, want},
			{"gateway_submissions", "credential_digest", f.digest, want},
			{"gateway_submission_references", "workspace_id", f.workspaceID, want},
		} {
			var count int
			err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+row.table+` WHERE `+row.key+`=?`, row.id).Scan(&count)
			if err != nil || count != row.count {
				t.Errorf("%s has %d records for %s, want %d: %v", row.table, count, row.id, row.count, err)
			}
		}
	}
	assertRecords(removed, 1)
	assertRecords(kept, 1)
	for _, opID := range []string{removed.operationID, kept.operationID} {
		// A pending operation blocks deletion whether the target owns the
		// Workspace or the operation is routed to the target.
		exec(`UPDATE operations SET status='queued' WHERE id=?`, opID)
		if _, err := s.DeleteHost(ctx, target.ID); !errors.Is(err, ErrHostHasPendingOps) {
			t.Fatalf("pending operation %s did not block deletion: %v", opID, err)
		}
		assertRecords(removed, 1)
		assertRecords(kept, 1)
		exec(`UPDATE operations SET status='succeeded' WHERE id=?`, opID)
	}
	if _, err := s.DeleteHost(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	assertRecords(removed, 0)
	assertRecords(kept, 1)
	if after, err := s.Grant(ctx, grant.ID); err != nil || !reflect.DeepEqual(after, grant) {
		t.Fatalf("shared grant changed after Host deletion: %v", err)
	}
	if liveAfter, err := s.AuthenticateCredential(ctx, liveCredential.Digest); err != nil || liveAfter != liveBefore {
		t.Fatalf("live Workspace lost admission through its shared grant: %v", err)
	}
	if settings, err := s.Settings(ctx); err != nil || settings.WorkspaceImage != image {
		t.Fatalf("global settings after Host deletion: %+v, %v", settings, err)
	}
}
