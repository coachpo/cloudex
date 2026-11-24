package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestFreshDatabaseUsesDefaultLocalOperationLimit(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	h, err := s.Host(ctx, LocalHostID)
	if err != nil || h.MaxOperationsTarget != 3 {
		t.Fatalf("fresh local Host target=%d, %v", h.MaxOperationsTarget, err)
	}
}

func TestHeartbeatPreservesHostOperationTarget(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	h := connectedHost(t, s, "worker", Resources{})
	if _, err := s.SetHostMaxOperations(ctx, h.ID, 7); err != nil {
		t.Fatal(err)
	}
	report := hostReport(0, 0)
	report.MaxOperations = 4
	if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
		t.Fatal(err)
	}
	h, err := s.Host(ctx, h.ID)
	if err != nil || h.MaxOperationsTarget != 7 || h.MaxOperations != 4 {
		t.Fatalf("Host limits after heartbeat: target=%d actual=%d, %v", h.MaxOperationsTarget, h.MaxOperations, err)
	}
}

func TestClaimOpUsesSmallerHostOperationLimit(t *testing.T) {
	for _, tc := range []struct {
		name           string
		target, actual int
	}{
		{"target lower than actual", 1, 2},
		{"actual lower than target", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := testStore(t), t.Context()
			h := connectedHost(t, s, "worker", Resources{CPU: 3000, Memory: 3 * gib, Disk: 3 * gib})
			for i := range 2 {
				if _, err := s.Create(ctx, hostWorkspace(h, fmt.Sprintf("workspace %d", i)), "", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.ClaimOp(ctx, true); err != nil {
				t.Fatal(err)
			}
			report := hostReport(h.Capacity.CPU, h.Capacity.Memory)
			report.MaxOperations = tc.actual
			if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetHostMaxOperations(ctx, h.ID, tc.target); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ClaimOp(ctx, true); !errors.Is(err, ErrMissing) {
				t.Fatalf("claimed a second operation with target=%d actual=%d: %v", tc.target, tc.actual, err)
			}
			if _, err := s.SetHostMaxOperations(ctx, h.ID, 2); err != nil {
				t.Fatal(err)
			}
			if tc.actual < 2 {
				if _, _, err := s.ClaimOp(ctx, true); !errors.Is(err, ErrMissing) {
					t.Fatalf("claimed before the Agent applied its new limit: %v", err)
				}
				report.MaxOperations = 2
				if err := s.Heartbeat(ctx, h.ID, "worker-key", report); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.ClaimOp(ctx, true); err != nil {
				t.Fatalf("second operation was not claimable once both limits reached two: %v", err)
			}
		})
	}
}
