package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func grantFixture(t *testing.T, db *Store, channel, clientID, upstreamWorkspace string) Grant {
	t.Helper()
	g := Grant{
		ID: ID(), Channel: channel, Issuer: "https://issuer.example.test", ClientID: clientID,
		Subject: "fixture-subject", UpstreamUserID: "fixture-user", UpstreamWorkspaceID: upstreamWorkspace,
		Email: "fixture@example.test", Status: GrantActive, Scope: "example",
		Credential: []byte("fixture-sealed-value"), ExpiresAt: time.Now().Truncate(time.Second).Add(time.Hour),
	}
	g.RegistrationID = GrantRegistrationID(g.Channel, g.Issuer, g.ClientID)
	if err := db.AddGrant(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	g, err := db.Grant(t.Context(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGrantRegistrationIdentityAndReauthorization(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	codex := grantFixture(t, db, "codex", "first-party", "upstream-1")
	other := grantFixture(t, db, "codex", "first-party", "upstream-2")
	chatgpt := grantFixture(t, db, "chatgpt", "issued-1", "")
	if codex.RegistrationID != other.RegistrationID || codex.RegistrationID == chatgpt.RegistrationID {
		t.Fatalf("unexpected registration scoping: %+v %+v %+v", codex.RegistrationID, other.RegistrationID, chatgpt.RegistrationID)
	}
	for name, grant := range map[string]Grant{"codex": codex, "chatgpt": chatgpt} {
		grant.ID = ID()
		if err := db.AddGrant(ctx, grant); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate %s: %v", name, err)
		}
	}
	otherChatGPT := chatgpt
	otherChatGPT.ID, otherChatGPT.Subject = ID(), "wrong-user"
	if err := db.AddGrant(ctx, otherChatGPT); !errors.Is(err, ErrConflict) {
		t.Fatalf("registration owned by another subject: %v", err)
	}
	for _, mutate := range []func(*Grant){
		func(g *Grant) { g.Subject = "wrong-user" },
		func(g *Grant) { g.UpstreamUserID = "wrong-user" },
		func(g *Grant) { g.UpstreamWorkspaceID = other.UpstreamWorkspaceID },
		func(g *Grant) {
			g.Channel, g.RegistrationID, g.ClientID = chatgpt.Channel, chatgpt.RegistrationID, chatgpt.ClientID
		},
	} {
		replacement := codex
		mutate(&replacement)
		if err := db.ReplaceGrant(ctx, replacement, codex.GrantEpoch); !errors.Is(err, ErrConflict) {
			t.Fatalf("mis-selected identity: %v", err)
		}
	}
	unchanged, err := db.Grant(ctx, codex.ID)
	if err != nil || unchanged.GrantEpoch != 1 || string(unchanged.Credential) != string(codex.Credential) {
		t.Fatalf("mismatched login modified authorization: epoch=%d err=%v", unchanged.GrantEpoch, err)
	}
	grants, err := db.Grants(ctx)
	if err != nil || len(grants) != 3 {
		t.Fatalf("grant list: %d %v", len(grants), err)
	}
	for _, grant := range grants {
		if grant.Credential != nil || grant.Email != codex.Email {
			t.Fatalf("grant list returned unexpected credential or email: grant_id=%s", grant.ID)
		}
	}
}

func TestGrantRefreshAttemptRecoveryFencesDuplicateAndLateResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh.sqlite")
	ctx := t.Context()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	a := grantFixture(t, db, "codex", "first-party", "upstream-a")
	b := grantFixture(t, db, "chatgpt", "issued-b", "")
	w1, c1 := bindRunning(t, db, "refresh-one", a)
	_, c2 := bindRunning(t, db, "refresh-two", a)
	_, c3 := bindRunning(t, db, "refresh-three", b)
	start := make(chan struct{})
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, attempt := range []string{"attempt-one", "attempt-two"} {
		group.Go(func() {
			<-start
			results <- result{attempt, db.BeginGrantRefresh(ctx, a.ID, a.GrantEpoch, a.TokenVersion, attempt)}
		})
	}
	close(start)
	group.Wait()
	close(results)
	var won string
	for outcome := range results {
		if outcome.err == nil && won == "" {
			won = outcome.id
		} else if !errors.Is(outcome.err, ErrConflict) {
			t.Fatalf("unexpected refresh admission: %v", outcome)
		}
	}
	if won == "" {
		t.Fatal("neither refresh attempt won")
	}
	newAccess := a
	newAccess.Credential = []byte("fixture-rotated")
	newAccess.RefreshRetryAt = a.ExpiresAt.Add(-45 * time.Second)
	if err := db.CompleteGrantRefresh(ctx, newAccess, won); err != nil {
		t.Fatal(err)
	}
	latest, err := db.Grant(ctx, a.ID)
	if err != nil || latest.GrantEpoch != a.GrantEpoch || latest.TokenVersion != a.TokenVersion+1 {
		t.Fatalf("normal refresh changed the epoch or missed rotation: epoch=%d version=%d %v", latest.GrantEpoch, latest.TokenVersion, err)
	}
	if !latest.RefreshRetryAt.Equal(newAccess.RefreshRetryAt) {
		t.Fatalf("successful refresh did not persist its retry time: %s", latest.RefreshRetryAt)
	}
	for _, cred := range []GatewayCredential{c1, c2} {
		if _, err := db.AuthenticateCredential(ctx, cred.Digest); err != nil {
			t.Fatalf("normal refresh invalidated workspace %s: %v", cred.WorkspaceID, err)
		}
	}
	const interrupted = "interrupted-before-save"
	if err := db.BeginGrantRefresh(ctx, a.ID, latest.GrantEpoch, latest.TokenVersion, interrupted); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverGrantRefreshes(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := db.Grant(ctx, a.ID)
	if err != nil || failed.Status != GrantReauthRequired || failed.Reason != "refresh_outcome_unknown" ||
		failed.GrantEpoch != latest.GrantEpoch+1 || failed.RefreshAttemptID != "" {
		t.Fatalf("interrupted refresh recovery: status=%s reason=%s err=%v", failed.Status, failed.Reason, err)
	}
	for _, cred := range []GatewayCredential{c1, c2} {
		got, err := db.AuthenticateCredential(ctx, cred.Digest)
		if !errors.Is(err, ErrGrantUnavailable) || got.GrantID != a.ID {
			t.Fatalf("affected workspace grant: workspace=%s grant=%s err=%v", cred.WorkspaceID, got.GrantID, err)
		}
	}
	if _, err := db.AuthenticateCredential(ctx, c3.Digest); err != nil {
		t.Fatalf("unrelated grant stopped: %v", err)
	}
	replacement := failed
	replacement.Status, replacement.Reason = GrantActive, ""
	if err := db.ReplaceGrant(ctx, replacement, failed.GrantEpoch); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteGrantRefresh(ctx, latest, interrupted); !errors.Is(err, ErrConflict) {
		t.Fatalf("late refresh response updated replacement: %v", err)
	}
	if err := db.FailGrantRefresh(ctx, a.ID, latest.GrantEpoch, latest.TokenVersion, interrupted, GrantReauthRequired, "late_failure"); !errors.Is(err, ErrConflict) {
		t.Fatalf("late refresh failure updated replacement: %v", err)
	}
	if _, err := db.AuthenticateCredential(ctx, c1.Digest); !errors.Is(err, ErrMissing) {
		t.Fatalf("old credential survived reauthorization: %v", err)
	}
	if binding, err := db.Binding(ctx, w1.ID); err != nil || binding.GrantID != a.ID {
		t.Fatalf("reauthorization changed workspace binding: %+v %v", binding, err)
	}
}

func TestDisconnectRevokesOnlyAffectedGrantAndFencesLateResult(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := grantFixture(t, db, "chatgpt", "issued", "")
	b := grantFixture(t, db, "codex", "first-party", "upstream-b")
	w1, c1 := bindRunning(t, db, "disconnect-one", a)
	_, c2 := bindRunning(t, db, "disconnect-two", b)
	epoch, err := db.DisconnectGrant(ctx, a.ID, a.GrantEpoch)
	if err != nil {
		t.Fatal(err)
	}
	disconnected, err := db.Grant(ctx, a.ID)
	if err != nil || disconnected.Status != GrantDisconnected || disconnected.Credential != nil ||
		disconnected.Reason != "remote_revocation_unconfirmed" || disconnected.GrantEpoch != epoch {
		t.Fatalf("unexpected local revocation state: status=%s reason=%s epoch=%d err=%v", disconnected.Status, disconnected.Reason, disconnected.GrantEpoch, err)
	}
	if _, err := db.AuthenticateCredential(ctx, c1.Digest); !errors.Is(err, ErrGrantUnavailable) {
		t.Fatalf("locally disconnected credential: %v", err)
	}
	if _, err := db.AuthenticateCredential(ctx, c2.Digest); err != nil {
		t.Fatalf("unrelated authorization: %v", err)
	}
	if err := db.DeleteGrant(ctx, a.ID); !errors.Is(err, ErrGrantReferenced) {
		t.Fatalf("referenced grant deletion: %v", err)
	}
	if err := db.ReplaceGrant(ctx, a, epoch); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishGrantRevocation(ctx, a.ID, epoch, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("late revoke result updated replacement: %v", err)
	}
	if _, err := db.SetBinding(ctx, w1.ID, "", c1.BindingRevision, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteGrant(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBoundCreateReceiptRemainsAvailableAfterGrantDisconnect(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	grant := grantFixture(t, db, "chatgpt", "issued", "")
	w := Workspace{ID: ID(), Name: "create-grant", Image: "fixture", Branch: "main", Desired: "running", CPU: 1000, Memory: 1024}
	created, err := db.CreateWithGrant(ctx, w, "create-key", "hash", nil, grant.ID, grant.GrantEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DisconnectGrant(ctx, grant.ID, grant.GrantEpoch); err != nil {
		t.Fatal(err)
	}
	if got, err := db.IdempotentOperation(ctx, "create-key", "hash"); err != nil || got.ID != created.ID {
		t.Fatalf("accepted creation lost its idempotency receipt: %+v %v", got, err)
	}
	if _, err := db.IdempotentOperation(ctx, "create-key", "changed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed request reused an idempotency key: %v", err)
	}
	if _, err := db.IdempotentOperation(ctx, "", ""); !errors.Is(err, ErrMissing) {
		t.Fatalf("request without idempotency key found a receipt: %v", err)
	}
}
