package store

import (
	"errors"
	"testing"
	"time"
)

func TestSSHAdmissionSerializesWorkspaceChanges(t *testing.T) {
	for _, change := range []string{"revoke", "stop"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			s := testStore(t)
			w, _ := makeRunning(t, s)
			const key = "fixture public key"
			if err := s.SetSSHKeys(ctx, []string{key}); err != nil {
				t.Fatal(err)
			}

			entered, release, admitted := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			defer func() { close(release) }()
			go func() {
				admitted <- s.AdmitSSH(ctx, w.ID, key, w.Generation, func() error {
					close(entered)
					<-release
					return nil
				})
			}()
			select {
			case <-entered:
			case err := <-admitted:
				t.Fatalf("authorized admission did not reach acceptance: %v", err)
			}

			other := w
			other.ID, other.Name = ID(), "independent workspace"
			if _, err := s.Create(ctx, other, "", "", nil); err != nil {
				t.Fatal("admission blocked the database:", err)
			}
			done := make(chan error, 1)
			go func() {
				if change == "revoke" {
					done <- s.SetSSHKeys(ctx, nil)
					return
				}
				_, err := s.Enqueue(ctx, w.ID, "stop", false, "", "")
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("change did not wait for acceptance: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			release <- struct{}{}
			if err := <-admitted; err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := s.AdmitSSH(ctx, w.ID, key, w.Generation, func() error {
				t.Error("accepted a new channel after workspace change")
				return nil
			}); !errors.Is(err, ErrBusy) {
				t.Fatalf("admission after workspace change: %v", err)
			}
		})
	}
}
