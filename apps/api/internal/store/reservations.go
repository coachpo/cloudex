package store

import (
	"context"
	"database/sql"
	"errors"
)

type Reservation struct {
	WorkspaceID string    `json:"workspace_id"`
	HostID      string    `json:"host_id"`
	Resources   Resources `json:"resources"`
	Status      string    `json:"status"`
	UpdatedAt   string    `json:"updated_at"`
}

const reservationFields = "workspace_id,host_id,cpu_milli,memory_bytes,disk_bytes,status,updated_at"

func scanReservation(r scanner) (v Reservation, err error) {
	err = missing(r.Scan(&v.WorkspaceID, &v.HostID, &v.Resources.CPU, &v.Resources.Memory, &v.Resources.Disk, &v.Status, &v.UpdatedAt))
	return
}

func (s *Store) HostReservations(ctx context.Context, id string) ([]Reservation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+reservationFields+` FROM workspace_reservations WHERE host_id=?1 ORDER BY updated_at DESC,workspace_id`, id)
	return collect(rows, err, scanReservation)
}

func (s *Store) WorkspaceReservation(ctx context.Context, id string) (Reservation, error) {
	return workspaceReservation(ctx, s.db, id)
}

func workspaceReservation(ctx context.Context, q queryer, id string) (Reservation, error) {
	return scanReservation(q.QueryRowContext(ctx, "SELECT "+reservationFields+" FROM workspace_reservations WHERE workspace_id=?", id))
}

func workspaceResources(w Workspace) Resources {
	return Resources{CPU: w.CPU, Memory: w.Memory * 1024 * 1024, Disk: w.Disk}
}

func placementAllowed(ctx context.Context, tx *sql.Tx, h Host, image string) error {
	if err := hostAcceptsPlacement(h.Scheduling); err != nil {
		return err
	}
	if h.Status != "online" {
		return ErrHostOffline
	}
	var ready bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM host_images WHERE host_id=? AND image=? AND ready=1)`, h.ID, image).Scan(&ready); err != nil {
		return err
	}
	if !ready {
		return ErrHostIncompatible
	}
	return nil
}

func hostAcceptsPlacement(state string) error {
	switch state {
	case "disabled":
		return ErrHostDisabled
	case "cordoned":
		return ErrHostCordoned
	default:
		return nil
	}
}

// Reserve and write the ledger in the same transaction that enqueues the
// operation. The counters track promises, independent of usage telemetry.
func reserveHost(ctx context.Context, tx *sql.Tx, w Workspace, checkPlacement bool) error {
	resources := workspaceResources(w)
	query := `UPDATE host_capacity SET cpu_reserved=cpu_reserved+?,memory_reserved=memory_reserved+?,disk_reserved=disk_reserved+? WHERE host_id=?`
	args := []any{resources.CPU, resources.Memory, resources.Disk, w.HostID}
	h, err := findHost(ctx, tx, w.HostID)
	if err != nil {
		return err
	}
	if checkPlacement {
		if err := placementAllowed(ctx, tx, h, w.Image); err != nil {
			return err
		}
		query += ` AND ?<=max(cpu_allocatable-cpu_reserved,0)
		 AND ?<=max(memory_allocatable-memory_reserved,0)
		 AND ?<=max(disk_allocatable-disk_reserved,0)`
		args = append(args, resources.CPU, resources.Memory, resources.Disk)
	} else if err := hostAcceptsPlacement(h.Scheduling); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err := affected(res, err, ErrConflict); !checkPlacement || !errors.Is(err, ErrConflict) {
		return err
	}
	switch {
	case resources.CPU > h.Available.CPU:
		return &CapacityError{w.HostID, "cpu", resources.CPU, h.Available.CPU}
	case resources.Memory > h.Available.Memory:
		return &CapacityError{w.HostID, "memory", resources.Memory, h.Available.Memory}
	default:
		return &CapacityError{w.HostID, "disk", resources.Disk, h.Available.Disk}
	}
}

func insertReservation(ctx context.Context, tx *sql.Tx, w Workspace) error {
	resources := workspaceResources(w)
	_, err := tx.ExecContext(ctx, `INSERT INTO workspace_reservations (`+reservationFields+`) VALUES(?,?,?,?,?,'reserved',?)`,
		w.ID, w.HostID, resources.CPU, resources.Memory, resources.Disk, now())
	return err
}

func resolveReservation(ctx context.Context, tx *sql.Tx, id string, keepDisk bool) error {
	v, err := workspaceReservation(ctx, tx, id)
	if err != nil {
		return err
	}
	if v.Status == "released" || (v.Status == "retained" && keepDisk) {
		return nil
	}
	released := v.Resources
	status := "released"
	if v.Status == "retained" {
		released.CPU, released.Memory = 0, 0
	}
	if keepDisk && released.Disk > 0 {
		status = "retained"
		released.Disk = 0
	}
	if _, err = tx.ExecContext(ctx, `UPDATE workspace_reservations SET status=?,updated_at=? WHERE workspace_id=?`, status, now(), id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE host_capacity SET cpu_reserved=cpu_reserved-?,memory_reserved=memory_reserved-?,disk_reserved=disk_reserved-? WHERE host_id=?`,
		released.CPU, released.Memory, released.Disk, v.HostID)
	return err
}

func reacquireReservation(ctx context.Context, tx *sql.Tx, w Workspace, o Operation) error {
	v, err := workspaceReservation(ctx, tx, w.ID)
	if err != nil || v.Status != "released" {
		return err
	}
	if err = reserveHost(ctx, tx, w, true); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE workspace_reservations SET status='reserved',updated_at=? WHERE workspace_id=?`, now(), w.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE operations SET reacquired_reservation=1 WHERE id=?`, o.ID)
	return err
}
