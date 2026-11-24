package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSSHAdmissionSerializesWorkspaceChanges(t *testing.T) {
	for _, change := range []string{"revoke", "stop"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			s := testStore(t)
			w, _ := makeRunning(t, s)
			const key = "fixture public key"
			op, err := s.SetSSH(ctx, w.ID, []string{key}, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FinishOp(ctx, op, "running", &Container{Generation: op.Target, ID: ID(), ImageID: "image", Status: "running"}, ""); err != nil {
				t.Fatal(err)
			}

			entered, release, admitted := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				admitted <- s.AdmitSSH(ctx, w.ID, key, op.Target, func() error {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-entered:
			case err := <-admitted:
				t.Fatalf("authorized admission did not reach acceptance: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			update := func(ctx context.Context, id string) error {
				if change == "revoke" {
					_, err := s.SetSSH(ctx, id, nil, "", "")
					return err
				}
				_, err := s.Enqueue(ctx, id, "stop", false, "", "")
				return err
			}
			other := w
			other.ID, other.Name = ID(), "independent workspace"
			otherOp, err := s.Create(ctx, other, "", "", nil, nil)
			if err != nil {
				t.Fatal("admission blocked the database:", err)
			}
			if err := s.FinishOp(ctx, otherOp, "running", &Container{Generation: 1, ID: ID(), ImageID: "image"}, ""); err != nil {
				t.Fatal(err)
			}
			if err := update(ctx, other.ID); err != nil {
				t.Fatal("admission blocked another workspace:", err)
			}
			blocked, stop := context.WithTimeout(ctx, 50*time.Millisecond)
			err = update(blocked, w.ID)
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("same-workspace change did not wait for acceptance: %v", err)
			}
			close(release)
			if err := <-admitted; err != nil {
				t.Fatal(err)
			}
			if err := update(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.AdmitSSH(ctx, w.ID, key, op.Target, func() error {
				t.Error("accepted a new channel after workspace change")
				return nil
			}); !errors.Is(err, ErrBusy) {
				t.Fatalf("admission after workspace change: %v", err)
			}
		})
	}
}

func TestSSHChangesUseLifecycleQueue(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	w, _ := makeRunning(t, s)
	update := []string{"test public key"}
	pending, e := s.Enqueue(ctx, w.ID, "stop", false, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SetSSH(ctx, w.ID, update, "", ""); !errors.Is(e, ErrBusy) {
		t.Fatalf("settings changed while Stop was queued: %v", e)
	}
	if e = s.CancelOp(ctx, pending.ID); e != nil {
		t.Fatal(e)
	}
	op, e := s.SetSSH(ctx, w.ID, update, "change-ssh", "data")
	if e != nil || !op.ApplySSH || op.Target != w.Generation+1 {
		t.Fatalf("enqueue update: %+v %v", op, e)
	}
	replay, e := s.SetSSH(ctx, w.ID, update, "change-ssh", "data")
	if e != nil || replay.ID != op.ID {
		t.Fatalf("idempotent access change: %+v %v", replay, e)
	}
	if e = s.CancelOp(ctx, op.ID); !errors.Is(e, ErrBusy) {
		t.Fatalf("cancelled access update after desired state was saved: %v", e)
	}
	_, access, busy, e := s.WorkspaceSSH(ctx, w.ID)
	if e != nil || !busy || !access.Enabled || len(access.Keys) != 1 {
		t.Fatalf("access not saved: %+v %v", access, e)
	}
	other := w
	other.ID = ID()
	other.Name = "another workspace"
	if _, e = s.Create(ctx, other, "", "", update, nil); e != nil {
		t.Fatalf("same public key can be authorized for another workspace: %v", e)
	}
	if e = s.FinishOp(ctx, op, "running", &Container{Generation: op.Target, ID: ID(), ImageID: "image", Status: "running"}, ""); e != nil {
		t.Fatal(e)
	}
	off, e := s.SetSSH(ctx, w.ID, nil, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FinishOp(ctx, off, "running", nil, ""); e != nil {
		t.Fatal(e)
	}
	_, access, _, e = s.WorkspaceSSH(ctx, w.ID)
	if e != nil || access.Enabled || len(access.Keys) > 0 {
		t.Fatalf("disable retained authorization: %+v %v", access, e)
	}
	del, e := s.Enqueue(ctx, w.ID, "delete", false, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FinishOp(ctx, del, "deleted", nil, ""); e != nil {
		t.Fatal(e)
	}
	if _, a, _, e := s.WorkspaceSSH(ctx, other.ID); e != nil || !a.Enabled {
		t.Fatalf("deleting one workspace affected the authorization of another: %+v %v", a, e)
	}
}
