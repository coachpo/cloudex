package store

import (
	"errors"
	"slices"
	"testing"
)

func TestFreshDatabaseStoresAndReplacesHostImages(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	local, err := s.Host(ctx, LocalHostID)
	if err != nil || local.Images == nil || len(local.Images) != 0 {
		t.Fatalf("fresh Host images = %+v, %v; want empty array", local.Images, err)
	}
	h := connectedHost(t, s, "worker", Resources{})
	report := hostReport(0, 0)
	report.Images = []ImageStatus{{Image: "workspace:z"}, {Image: "workspace:a", Ready: true}}
	if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
		t.Fatal(err)
	}
	got, err := s.Host(ctx, h.ID)
	want := []ImageStatus{report.Images[1], report.Images[0]}
	if err != nil || !slices.Equal(got.Images, want) {
		t.Fatalf("Host images after replacement = %+v, %v; want %+v", got.Images, err, want)
	}
	report.Images = []ImageStatus{}
	if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
		t.Fatal(err)
	}
	got, err = s.Host(ctx, h.ID)
	if err != nil || got.Images == nil || len(got.Images) != 0 {
		t.Fatalf("Host images after empty heartbeat = %+v, %v; want empty array", got.Images, err)
	}
}

func TestDesiredWorkspaceImagesIncludeOnlyCurrentDefaultAndLiveWorkspaces(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	if got, err := s.DesiredWorkspaceImages(ctx); err != nil || !slices.Equal(got, []string{DefaultWorkspaceImage}) {
		t.Fatalf("initial desired images = %v, %v", got, err)
	}
	const next = "workspace:next"
	if err := s.SetWorkspaceImage(ctx, next); err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"workspace:z", next, "workspace:z", "workspace:deleted"} {
		id := ID()
		w := Workspace{ID: id, Name: id, Image: image, Branch: "main", Desired: "stopped", CPU: 1000, Memory: 1024}
		if _, err := s.Create(ctx, w, "", "", nil); err != nil {
			t.Fatal(err)
		}
		if image == "workspace:deleted" {
			if _, err := s.db.ExecContext(ctx, "UPDATE workspaces SET deleted_at=? WHERE id=?", now(), id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, err := s.DesiredWorkspaceImages(ctx); err != nil || !slices.Equal(got, []string{next, "workspace:z"}) {
		t.Fatalf("desired images after changing default = %v, %v", got, err)
	}
}

func TestRebuildAndResetReacquireUsingRecordedImage(t *testing.T) {
	for _, operation := range []string{"rebuild", "reset"} {
		t.Run(operation, func(t *testing.T) {
			s, ctx := testStore(t), t.Context()
			capacity := Resources{CPU: 4000, Memory: 4 * gib, Disk: 4 * gib}
			h := connectedHost(t, s, "worker", capacity)
			w := hostWorkspace(h, "old Workspace")
			create, err := s.Create(ctx, w, "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FailCleanCreate(ctx, create, "no artifacts"); err != nil {
				t.Fatal(err)
			}
			const next = "workspace:next"
			if err := s.SetWorkspaceImage(ctx, next); err != nil {
				t.Fatal(err)
			}
			report := hostReport(capacity.CPU, capacity.Memory)
			report.Images = []ImageStatus{{Image: next, Ready: true}}
			if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Enqueue(ctx, w.ID, operation, false, "", ""); !errors.Is(err, ErrHostIncompatible) {
				t.Fatalf("%s reacquired a reservation without the recorded image: %v", operation, err)
			}
			report.Images = []ImageStatus{{Image: w.Image, Ready: true}}
			if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Enqueue(ctx, w.ID, operation, false, "", ""); err != nil {
				t.Fatal(err)
			}
			if reservation, err := s.WorkspaceReservation(ctx, w.ID); err != nil || reservation.Status != "reserved" {
				t.Fatalf("%s did not reacquire reservation: %+v %v", operation, reservation, err)
			}
		})
	}
}
