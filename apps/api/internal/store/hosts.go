package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const LocalHostID = "local"
const HostLease = 30 * time.Second

var (
	ErrInvalidEnrollment = errors.New("host enrollment is invalid or expired")
	ErrHostUnauthorized  = errors.New("host identity is no longer authorized")
	ErrHostOffline       = errors.New("selected host is unreachable")
	ErrHostCordoned      = errors.New("selected host is cordoned")
	ErrHostDisabled      = errors.New("selected host is disabled")
	ErrHostIncompatible  = errors.New("selected host does not have the workspace image")
	ErrHostMode          = errors.New("invalid host mode")
)

type Resources struct {
	CPU    int64 `json:"cpu_milli"`
	Memory int64 `json:"memory_bytes"`
	Disk   int64 `json:"disk_bytes"`
}

type ImageStatus struct {
	Image string `json:"image"`
	Ready bool   `json:"ready"`
}

type HostReport struct {
	AgentVersion      string        `json:"agent_version"`
	Engine            string        `json:"engine"`
	OS                string        `json:"os"`
	Arch              string        `json:"arch"`
	Images            []ImageStatus `json:"images"`
	Healthy           bool          `json:"healthy"`
	CPU               int64         `json:"-"`
	Memory            int64         `json:"-"`
	MaxOperations     int           `json:"max_operations"`
	RunningContainers int           `json:"running_containers"`
	TotalContainers   int           `json:"total_containers"`
}

type Host struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Status              string    `json:"status"`
	Scheduling          string    `json:"scheduling_state"`
	SchedulingVersion   int64     `json:"scheduling_version"`
	Reachable           bool      `json:"reachable"`
	CreatedAt           string    `json:"created_at"`
	LastSeen            *string   `json:"last_seen"`
	Capacity            Resources `json:"allocatable"`
	Reserved            Resources `json:"reserved"`
	Available           Resources `json:"available"`
	MaxOperationsTarget int       `json:"max_operations_target"`
	ConnectionType      string    `json:"connection_type"`
	SSHAddress          string    `json:"ssh_address,omitempty"`
	SSHUser             string    `json:"ssh_user,omitempty"`
	SSHHostKey          string    `json:"ssh_host_key,omitempty"`
	SSHRevision         int64     `json:"ssh_revision,omitempty"`
	ConnectionError     string    `json:"connection_error,omitempty"`
	certificate         string
	HostReport
}

const hostFields = `h.id,h.name,h.scheduling_state,h.scheduling_version,h.created_at,h.last_seen,h.last_heartbeat_unix,coalesce(h.certificate_serial,''),
 h.connection_type,h.ssh_address,h.ssh_user,h.ssh_host_key,h.ssh_revision,h.connection_error,
 h.agent_version,h.engine,h.os,h.arch,h.healthy,h.max_operations,h.max_operations_target,h.running_containers,h.total_containers,
 (SELECT json_group_array(json_object('image',image,'ready',json(CASE WHEN ready THEN 'true' ELSE 'false' END)) ORDER BY image)
  FROM host_images WHERE host_id=h.id),
 c.cpu_allocatable,c.memory_allocatable,c.disk_allocatable,c.cpu_reserved,c.memory_reserved,c.disk_reserved`

const hostQuery = "SELECT " + hostFields + " FROM host_nodes h JOIN host_capacity c ON c.host_id=h.id"

// Compare against time received by the control plane so agent clock skew
// cannot make a stale lease eligible for scheduling.
func hostCutoff() int64 { return time.Now().Add(-HostLease).Unix() }

func scanHost(r scanner) (h Host, err error) {
	var last int64
	var images string
	err = missing(r.Scan(&h.ID, &h.Name, &h.Scheduling, &h.SchedulingVersion, &h.CreatedAt, &h.LastSeen, &last, &h.certificate,
		&h.ConnectionType, &h.SSHAddress, &h.SSHUser, &h.SSHHostKey, &h.SSHRevision, &h.ConnectionError,
		&h.AgentVersion, &h.Engine, &h.OS, &h.Arch, &h.Healthy, &h.MaxOperations, &h.MaxOperationsTarget, &h.RunningContainers, &h.TotalContainers, &images,
		&h.Capacity.CPU, &h.Capacity.Memory, &h.Capacity.Disk, &h.Reserved.CPU, &h.Reserved.Memory, &h.Reserved.Disk))
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(images), &h.Images); err != nil {
		return
	}
	h.Reachable = last > hostCutoff()
	switch {
	case h.ConnectionType == "agent" && h.certificate == "":
		h.Status = "enrolling"
	case !h.Reachable:
		h.Status = "offline"
	case !h.Healthy:
		h.Status = "degraded"
	default:
		h.Status = "online"
	}
	h.Available = Resources{
		CPU:    max(0, h.Capacity.CPU-h.Reserved.CPU),
		Memory: max(0, h.Capacity.Memory-h.Reserved.Memory),
		Disk:   max(0, h.Capacity.Disk-h.Reserved.Disk),
	}
	return
}

func (s *Store) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, hostQuery+" ORDER BY (h.id='local') DESC,h.name,h.id")
	return collect(rows, err, scanHost)
}

func findHost(ctx context.Context, q queryer, id string) (Host, error) {
	return scanHost(q.QueryRowContext(ctx, hostQuery+" WHERE h.id=?", id))
}

func (s *Store) Host(ctx context.Context, id string) (Host, error) {
	return findHost(ctx, s.db, id)
}

func (s *Store) HostMaxOperationsTarget(ctx context.Context, id string) (int, error) {
	var target int
	err := s.db.QueryRowContext(ctx, "SELECT max_operations_target FROM host_nodes WHERE id=?", id).Scan(&target)
	return target, missing(err)
}

func (s *Store) ExpireHostLease(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE host_nodes SET last_heartbeat_unix=0 WHERE (?='' OR id=?) AND last_heartbeat_unix<>0`, id, id)
	return err
}

func (s *Store) AddHost(ctx context.Context, name, tokenHash string, expires time.Time) (h Host, err error) {
	id := ID()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO host_nodes(id,name,created_at) VALUES(?,?,?) ON CONFLICT(name) DO NOTHING`, id, name, now())
		if err := affected(res, err, ErrConflict); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO host_capacity(host_id) VALUES(?)", id); err != nil {
			return err
		}
		if err := insertEnrollment(ctx, tx, id, tokenHash, expires); err != nil {
			return err
		}
		h, err = findHost(ctx, tx, id)
		return err
	})
	return
}

func insertEnrollment(ctx context.Context, tx *sql.Tx, id, tokenHash string, expires time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO host_enrollment_tokens(token_hash,host_id,expires_unix) VALUES(?,?,?)`, tokenHash, id, expires.Unix())
	return err
}

func (s *Store) RenewEnrollment(ctx context.Context, id, tokenHash string, expires time.Time) (h Host, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		h, err = findHost(ctx, tx, id)
		if err != nil {
			return err
		}
		if id == LocalHostID || h.ConnectionType != "agent" {
			return ErrInvalidEnrollment
		}
		if h.Scheduling == "disabled" {
			return ErrHostDisabled
		}
		if _, err = tx.ExecContext(ctx, `UPDATE host_enrollment_tokens SET revoked_at=? WHERE host_id=? AND used_at IS NULL AND revoked_at IS NULL`, now(), id); err != nil {
			return err
		}
		return insertEnrollment(ctx, tx, id, tokenHash, expires)
	})
	return
}

const enrollmentValid = "e.used_at IS NULL AND e.revoked_at IS NULL AND e.expires_unix>unixepoch()"

func (s *Store) PendingHost(ctx context.Context, tokenHash string) (Host, error) {
	h, err := scanHost(s.db.QueryRowContext(ctx, hostQuery+`
 JOIN host_enrollment_tokens e ON e.host_id=h.id
 WHERE e.token_hash=? AND h.scheduling_state<>'disabled' AND `+enrollmentValid, tokenHash))
	if errors.Is(err, ErrMissing) {
		err = ErrInvalidEnrollment
	}
	return h, err
}

func (s *Store) EnrollHost(ctx context.Context, id, tokenHash, serial string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE host_enrollment_tokens AS e SET used_at=?
 WHERE e.host_id=? AND e.token_hash=? AND `+enrollmentValid, now(), id, tokenHash)
		if err := affected(res, err, ErrInvalidEnrollment); err != nil {
			return err
		}
		return setHostCertificate(ctx, tx, id, serial)
	})
}

func (s *Store) SetLocalHostCertificate(ctx context.Context, serial string) error {
	return setHostCertificate(ctx, s.db, LocalHostID, serial)
}

func setHostCertificate(ctx context.Context, db executor, id, serial string) error {
	res, err := db.ExecContext(ctx, `UPDATE host_nodes SET
 last_heartbeat_unix=CASE WHEN certificate_serial=? THEN last_heartbeat_unix ELSE 0 END,
 certificate_serial=? WHERE id=? AND connection_type='agent' AND scheduling_state<>'disabled'`, serial, serial, id)
	return affected(res, err, ErrHostUnauthorized)
}

func (s *Store) AuthenticateHost(ctx context.Context, id, serial string) error {
	var matches bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM host_nodes WHERE id=? AND connection_type='agent' AND certificate_serial=? AND scheduling_state<>'disabled')`, id, serial).Scan(&matches)
	if err == nil && !matches {
		return ErrHostUnauthorized
	}
	return err
}

func (s *Store) Heartbeat(ctx context.Context, id, serial string, report HostReport) error {
	return s.heartbeat(ctx, id, "connection_type='agent' AND certificate_serial=?", serial, report)
}

func (s *Store) heartbeat(ctx context.Context, id, identityPredicate string, identity any, report HostReport) error {
	if report.CPU < 0 || report.Memory < 0 ||
		report.MaxOperations < 1 || report.RunningContainers < 0 || report.TotalContainers < report.RunningContainers {
		return errors.New("invalid host capacity report")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE host_nodes SET connection_error='',agent_version=?,engine=?,os=?,arch=?,healthy=?,
 max_operations=?,running_containers=?,total_containers=?,last_seen=?,last_heartbeat_unix=unixepoch()
 WHERE id=? AND scheduling_state<>'disabled' AND `+identityPredicate,
			report.AgentVersion, report.Engine, report.OS, report.Arch, report.Healthy,
			report.MaxOperations, report.RunningContainers, report.TotalContainers, now(), id, identity)
		if err := affected(res, err, ErrHostUnauthorized); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE host_capacity SET cpu_allocatable=?,memory_allocatable=? WHERE host_id=?`,
			report.CPU, report.Memory, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM host_images WHERE host_id=?`, id); err != nil {
			return err
		}
		for _, image := range report.Images {
			if _, err := tx.ExecContext(ctx, `INSERT INTO host_images(host_id,image,ready) VALUES(?,?,?)`, id, image.Image, image.Ready); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SetHostDisk(ctx context.Context, id string, bytes int64) (h Host, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE host_capacity SET disk_allocatable=? WHERE host_id=?`, bytes, id)
		if err := affected(res, err, ErrMissing); err != nil {
			return err
		}
		h, err = findHost(ctx, tx, id)
		return err
	})
	return
}

func (s *Store) SetHostMaxOperations(ctx context.Context, id string, n int) (h Host, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE host_nodes SET max_operations_target=? WHERE id=?", n, id)
		if err := affected(res, err, ErrMissing); err != nil {
			return err
		}
		h, err = findHost(ctx, tx, id)
		return err
	})
	return
}

func (s *Store) SetHostScheduling(ctx context.Context, id, state string) (h Host, err error) {
	switch state {
	case "enabled", "cordoned", "disabled":
	default:
		return h, ErrHostMode
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE host_nodes SET
 ssh_revision=ssh_revision+(connection_type='ssh' AND ?='disabled' AND scheduling_state<>'disabled'),
 scheduling_version=scheduling_version+(scheduling_state<>?), scheduling_state=?,
 last_heartbeat_unix=CASE WHEN ?='disabled' THEN 0 ELSE last_heartbeat_unix END WHERE id=?`,
			state, state, state, state, id)
		if err := affected(res, err, ErrMissing); err != nil {
			return err
		}
		if state == "disabled" {
			if _, err := tx.ExecContext(ctx, `UPDATE host_enrollment_tokens SET revoked_at=? WHERE host_id=? AND used_at IS NULL AND revoked_at IS NULL`, now(), id); err != nil {
				return err
			}
		}
		h, err = findHost(ctx, tx, id)
		return err
	})
	return
}

func affected(res sql.Result, err, notAffected error) error {
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err == nil && count == 0 {
		return notAffected
	}
	return err
}

type CapacityError struct {
	HostID    string `json:"host_id"`
	Resource  string `json:"resource"`
	Requested int64  `json:"requested"`
	Available int64  `json:"available"`
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("not enough %s capacity on selected host", e.Resource)
}
