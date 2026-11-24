package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const gib = 1024 * 1024 * 1024

func hostReport(cpu, memory int64) HostReport {
	return HostReport{Healthy: true, Images: []ImageStatus{{Image: "workspace", Ready: true}}, CPU: cpu, Memory: memory, MaxOperations: 1}
}

func connectedHost(t *testing.T, s *Store, name string, capacity Resources) Host {
	t.Helper()
	ctx := t.Context()
	h, err := s.AddHost(ctx, name, name+"-token", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollHost(ctx, h.ID, name+"-token", name+"-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, h.ID, name+"-key", hostReport(capacity.CPU, capacity.Memory)); err != nil {
		t.Fatal(err)
	}
	h, err = s.SetHostDisk(ctx, h.ID, capacity.Disk)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func hostWorkspace(host Host, name string) Workspace {
	return Workspace{
		ID: ID(), HostID: host.ID, Name: name, Image: "workspace", Branch: "main",
		Desired: "running", CPU: 1000, Memory: 1024, Disk: gib,
	}
}

func TestHostEnrollmentIsBoundSingleUseAndRenewable(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	expires := time.Now().Add(time.Minute)
	h, err := s.AddHost(ctx, "build worker", "enrollment-hash", expires)
	if err != nil || h.Status != "enrolling" {
		t.Fatalf("add host: %+v, %v", h, err)
	}
	pending, err := s.PendingHost(ctx, "enrollment-hash")
	if err != nil || pending.ID != h.ID {
		t.Fatalf("find pending enrollment: %+v, %v", pending, err)
	}
	if err := s.EnrollHost(ctx, LocalHostID, "enrollment-hash", "wrong-host-key"); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("used token for another host: %v", err)
	}
	type enrollment struct {
		key string
		err error
	}
	results := make(chan enrollment, 2)
	var contenders sync.WaitGroup
	for _, key := range []string{"first-key", "second-key"} {
		contenders.Go(func() { results <- enrollment{key, s.EnrollHost(ctx, h.ID, "enrollment-hash", key)} })
	}
	contenders.Wait()
	close(results)
	winner := ""
	for result := range results {
		if result.err == nil && winner == "" {
			winner = result.key
		} else if !errors.Is(result.err, ErrInvalidEnrollment) {
			t.Fatalf("consume enrollment: %+v", result)
		}
	}
	if winner == "" {
		t.Fatal("no host enrolled")
	}
	report := hostReport(2500, 8*gib)
	if err := s.Heartbeat(ctx, h.ID, winner, report); err != nil {
		t.Fatal(err)
	}
	h, err = s.Host(ctx, h.ID)
	if err != nil || h.Status != "online" || h.Available != (Resources{CPU: report.CPU, Memory: report.Memory}) || !h.Reachable || h.LastSeen == nil {
		t.Fatalf("heartbeat: %+v %v", h, err)
	}
	if _, err := s.RenewEnrollment(ctx, h.ID, "superseded-hash", expires); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewEnrollment(ctx, h.ID, "renewed-hash", expires); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingHost(ctx, "superseded-hash"); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("superseded token stayed valid: %v", err)
	}
	if err := s.AuthenticateHost(ctx, h.ID, winner); err != nil {
		t.Fatalf("issuing a new token revoked the current host prematurely: %v", err)
	}
	if err := s.EnrollHost(ctx, h.ID, "renewed-hash", "new-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, h.ID, winner, report); !errors.Is(err, ErrHostUnauthorized) {
		t.Fatalf("old key retained heartbeat authority: %v", err)
	}
	h, err = s.Host(ctx, h.ID)
	if err != nil || h.Status != "offline" || h.Reachable || h.LastSeen == nil {
		t.Fatalf("re-enrollment inherited old lease or dropped last seen: %+v %v", h, err)
	}
	if _, err := s.AddHost(ctx, "spare", "expired-token", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingHost(ctx, "expired-token"); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("expired token resolved: %v", err)
	}
}

func TestConfiguredHostDiskSurvivesHeartbeat(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	h := connectedHost(t, s, "worker", Resources{CPU: 4000, Memory: 8 * gib, Disk: 12 * gib})
	if err := s.Heartbeat(ctx, h.ID, "worker-key", hostReport(5000, 10*gib)); err != nil {
		t.Fatal(err)
	}
	h, err := s.Host(ctx, h.ID)
	want := Resources{CPU: 5000, Memory: 10 * gib, Disk: 12 * gib}
	if err != nil || h.Capacity != want || h.Available != want {
		t.Fatalf("capacity after heartbeat = %+v, available = %+v, err = %v; want %+v", h.Capacity, h.Available, err, want)
	}
}

func TestReservationsAtomicallyLimitConcurrentPlacement(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	h := connectedHost(t, s, "small", Resources{CPU: 4000, Memory: 8 * gib, Disk: 8 * gib})
	type create struct {
		o   Operation
		w   Workspace
		key string
		err error
	}
	var workers sync.WaitGroup
	results := make(chan create, 32)
	for i := range cap(results) {
		workers.Go(func() {
			key := fmt.Sprintf("workspace-%d", i)
			w := hostWorkspace(h, key)
			w.Memory = 6144
			o, err := s.Create(ctx, w, key, "hash", nil)
			results <- create{o, w, key, err}
		})
	}
	workers.Wait()
	close(results)
	var winner create
	for result := range results {
		if result.err == nil && winner.o.ID == "" {
			winner = result
			continue
		}
		var capacity *CapacityError
		if !errors.As(result.err, &capacity) || capacity.Resource != "memory" ||
			capacity.HostID != h.ID || capacity.Requested != 6*gib || capacity.Available != 2*gib {
			t.Fatalf("unexpected placement result: %+v", result)
		}
	}
	if winner.o.ID == "" {
		t.Fatal("no workspace won capacity")
	}
	h, err := s.Host(ctx, h.ID)
	if err != nil || h.Reserved != workspaceResources(winner.w) {
		t.Fatalf("unexpected capacity counters: %+v %v", h, err)
	}
	reservations, err := s.HostReservations(ctx, h.ID)
	if err != nil || len(reservations) != 1 || reservations[0].WorkspaceID != winner.w.ID || reservations[0].Status != "reserved" {
		t.Fatalf("unexpected ledger: %+v %v", reservations, err)
	}
	if _, err := s.SetHostScheduling(ctx, h.ID, "cordoned"); err != nil {
		t.Fatal(err)
	}
	if retry, err := s.Create(ctx, winner.w, winner.key, "hash", nil); err != nil || retry.ID != winner.o.ID {
		t.Fatalf("idempotent replay attempted a second reservation: %+v %v", retry, err)
	}
	if _, err := s.Create(ctx, winner.w, winner.key, "different hash", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key reused with different request: %v", err)
	}
	if _, err := s.Create(ctx, hostWorkspace(h, "new"), "", "", nil); !errors.Is(err, ErrHostCordoned) {
		t.Fatalf("accepted placement while cordoned: %v", err)
	}
}

func TestHostDispatchSkipsOfflineAndAppliesPerHostConcurrency(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	capacity := Resources{CPU: 4000, Memory: 4 * gib, Disk: 4 * gib}
	a := connectedHost(t, s, "A", capacity)
	b := connectedHost(t, s, "B", capacity)
	create := func(h Host, name string) Operation {
		op, err := s.Create(ctx, hostWorkspace(h, name), "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	a1 := create(a, "A first")
	a2 := create(a, "A second")
	b1 := create(b, "B first")
	if _, err := s.db.ExecContext(ctx, "UPDATE host_nodes SET last_heartbeat_unix=0 WHERE id=?", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, hostWorkspace(b, "B offline"), "", "", nil); !errors.Is(err, ErrHostOffline) {
		t.Fatalf("placed new workspace on expired host: %v", err)
	}
	claim := func(want Operation) Operation {
		t.Helper()
		op, w, err := s.ClaimOp(ctx, true)
		if err != nil || op.ID != want.ID || w.HostID != want.HostID {
			t.Fatalf("claim: %+v workspace=%+v err=%v, expected %+v", op, w, err, want)
		}
		return op
	}
	a1 = claim(a1)
	if _, _, err := s.ClaimOp(ctx, true); !errors.Is(err, ErrMissing) {
		t.Fatalf("claimed work for a busy or offline host: %v", err)
	}
	if err := s.Heartbeat(ctx, b.ID, "B-key", hostReport(capacity.CPU, capacity.Memory)); err != nil {
		t.Fatal(err)
	}
	b1 = claim(b1)
	if err := s.RetryOp(ctx, b1.ID, "Agent disconnected"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelOp(ctx, b1.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("previously dispatched create was cancelled: %v", err)
	}
	if err := s.FinishOp(ctx, a1, "running", &Container{Generation: 1, ID: "A container", Status: "running"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHostScheduling(ctx, a1.HostID, "cordoned"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, hostWorkspace(a, "A while cordoned"), "", "", nil); !errors.Is(err, ErrHostCordoned) {
		t.Fatalf("placed new workspace on a cordoned host: %v", err)
	}
	claim(a2)
	if _, _, err := s.ClaimOp(ctx, true); !errors.Is(err, ErrMissing) {
		t.Fatalf("retried without backoff: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE operations SET retry_after=0 WHERE id=?", b1.ID); err != nil {
		t.Fatal(err)
	}
	retried := claim(b1)
	if retried.Target != b1.Target || retried.Attempts != 2 {
		t.Fatalf("retry used a different generation or lost attempt count: %+v", retried)
	}
	if err := s.FailCleanCreate(ctx, b1, "late result"); err != nil {
		t.Fatal(err)
	}
	row, err := s.WorkspaceReservation(ctx, retried.WorkspaceID)
	if err != nil || row.Status != "reserved" {
		t.Fatalf("stale result released the active reservation: %+v %v", row, err)
	}
}

func TestReservationSettlementRetainsDiskAndRecoversCleanFailure(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	h := connectedHost(t, s, "vm", Resources{CPU: 4000, Memory: 4 * gib, Disk: 4 * gib})
	check := func(w Workspace, want string) {
		t.Helper()
		r, err := s.WorkspaceReservation(ctx, w.ID)
		if err != nil || r.Status != want {
			t.Fatalf("reservation for %q: %+v %v, want %s", w.Name, r, err, want)
		}
	}
	settle := func(w Workspace, kind, state string, purge bool, container *Container) {
		t.Helper()
		op, err := s.Enqueue(ctx, w.ID, kind, purge, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishOp(ctx, op, state, container, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishOp(ctx, op, state, container, ""); err != nil {
			t.Fatalf("repeated settlement: %v", err)
		}
	}
	w := hostWorkspace(h, "retain disk")
	create, err := s.Create(ctx, w, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishOp(ctx, create, "", nil, "cleanup unknown"); err != nil {
		t.Fatal(err)
	}
	check(w, "reserved")
	container := &Container{Generation: 1, ID: "first container", Status: "running"}
	settle(w, "rebuild", "running", false, container)
	check(w, "committed")
	container.Status = "exited"
	settle(w, "stop", "stopped", false, container)
	check(w, "committed")
	settle(w, "delete", "deleted", false, nil)
	check(w, "retained")
	h, err = s.Host(ctx, h.ID)
	if err != nil || h.Reserved != (Resources{Disk: w.Disk}) {
		t.Fatalf("delete released retained disk or held CPU/memory: %+v %v", h, err)
	}

	w = hostWorkspace(h, "clean failure")
	create, err = s.Create(ctx, w, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FailCleanCreate(ctx, create, "no artifacts"); err != nil {
		t.Fatal(err)
	}
	check(w, "released")
	rebuild, err := s.Enqueue(ctx, w.ID, "rebuild", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	check(w, "reserved")
	if err := s.CancelOp(ctx, rebuild.ID); err != nil {
		t.Fatal(err)
	}
	check(w, "released")
	container = &Container{Generation: 1, ID: "second container", Status: "running"}
	settle(w, "rebuild", "running", false, container)
	check(w, "committed")
	settle(w, "delete", "deleted", true, nil)
	check(w, "released")

	w = hostWorkspace(h, "cancel create")
	create, err = s.Create(ctx, w, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelOp(ctx, create.ID); err != nil {
		t.Fatal(err)
	}
	check(w, "released")
	if _, err := s.Workspace(ctx, w.ID); !errors.Is(err, ErrMissing) {
		t.Fatalf("cancelled create left workspace active: %v", err)
	}
}
