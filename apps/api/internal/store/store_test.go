package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.Context(), filepath.Join(t.TempDir(), "test.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func makeRunning(t *testing.T, s *Store) (Workspace, Operation) {
	t.Helper()
	ctx := t.Context()
	if err := s.SetLocalHostCertificate(ctx, "local-fixture-key"); err != nil {
		t.Fatal(err)
	}
	report := hostReport(4000, 8*gib)
	report.Images[0].Image = "local"
	if err := s.Heartbeat(ctx, LocalHostID, "local-fixture-key", report); err != nil {
		t.Fatal(err)
	}
	w := Workspace{ID: ID(), HostID: LocalHostID, Name: "backend", Image: "local", Branch: "main", Desired: "running", CPU: 1000, Memory: 1024}
	queued, e := s.Create(ctx, w, "create-01", "hash", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	retry, e := s.Create(ctx, w, "create-01", "hash", nil, nil)
	if e != nil || retry.ID != queued.ID {
		t.Fatalf("retry: %+v %v", retry, e)
	}
	if _, e = s.Create(ctx, w, "create-01", "different", nil, nil); !errors.Is(e, ErrConflict) {
		t.Fatalf("expected conflict, got %v", e)
	}
	duplicate := w
	duplicate.Name = "Backend"
	duplicate.ID = ID()
	if _, e = s.Create(ctx, duplicate, "", "", nil, nil); !errors.Is(e, ErrConflict) {
		t.Fatalf("expected name conflict, got %v", e)
	}
	o, _, e := s.ClaimOp(ctx)
	if e != nil || o.ID != queued.ID {
		t.Fatalf("claim: %+v %v", o, e)
	}
	if e = s.FinishOp(ctx, o, "running", &Container{Generation: 1, ID: ID(), ImageID: "image", Status: "running"}, ""); e != nil {
		t.Fatal(e)
	}
	w, e = s.Workspace(ctx, w.ID)
	if e != nil {
		t.Fatal(e)
	}
	return w, o
}

func TestExternalStopAllowsRestartAndRejectsStaleObservation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := makeRunning(t, s)
	if e := s.Observe(ctx, w, "stopped", "external stop", false, false); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Enqueue(ctx, w.ID, "start", false, "", ""); e != nil {
		t.Fatalf("restart blocked: %v", e)
	}
	// A stale sample cannot override a queued lifecycle change.
	if e := s.Observe(ctx, w, "running", "", false, false); e != nil {
		t.Fatal(e)
	}
	w, e := s.Workspace(ctx, w.ID)
	if e != nil || w.Observed != "stopped" {
		t.Fatalf("workspace: %+v %v", w, e)
	}
}
