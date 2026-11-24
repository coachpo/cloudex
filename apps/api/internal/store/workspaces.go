package store

import (
	"context"
	"database/sql"
	"errors"
)

type Workspace struct {
	ID                 string `json:"id"`
	HostID             string `json:"assigned_host_id"`
	Name               string `json:"name"`
	Image              string `json:"image"`
	RepoURL            string `json:"repo_url"`
	Branch             string `json:"branch"`
	CPU                int64  `json:"cpu_milli"`
	Memory             int64  `json:"memory_mb"`
	Disk               int64  `json:"disk_bytes"`
	Desired            string `json:"desired_state"`
	Observed           string `json:"observed_state"`
	Paused             bool   `json:"paused"`
	StartUnverified    bool   `json:"-"`
	ObservationVersion int64  `json:"-"`
	Generation         int    `json:"generation"`
	Error              string `json:"error"`
	UpdatedAt          string `json:"updated_at"`
}

type Git struct {
	Head      string `json:"head"`
	Status    string `json:"status"`
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated"`
}

type Container struct {
	HostID     string `json:"host_id"`
	Generation int    `json:"generation"`
	ID         string `json:"id"`
	ImageID    string `json:"image_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

type Operation struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	HostID      string  `json:"host_id"`
	Type        string  `json:"type"`
	Status      string  `json:"status"`
	Target      int     `json:"target_generation"`
	Purge       bool    `json:"purge"`
	ApplySSH    bool    `json:"apply_ssh"`
	Phase       int     `json:"-"`
	Attempts    int     `json:"attempts"`
	Error       string  `json:"error"`
	CreatedAt   string  `json:"created_at"`
	FinishedAt  *string `json:"finished_at"`
}

const workspaceFields = "id,name,image,repo_url,branch,cpu_milli,memory_mb,desired_state,observed_state,paused,start_unverified,generation,error,updated_at,observation_version,assigned_host_id,disk_bytes"
const operationFields = "id,workspace_id,type,status,target_generation,purge,apply_ssh,phase,attempts,error,created_at,finished_at,host_id"
const containerFields = "generation,docker_id,image_id,status,created_at,host_id"

func scanWorkspace(r scanner) (w Workspace, err error) {
	err = missing(r.Scan(&w.ID, &w.Name, &w.Image, &w.RepoURL, &w.Branch, &w.CPU, &w.Memory, &w.Desired, &w.Observed, &w.Paused, &w.StartUnverified, &w.Generation, &w.Error, &w.UpdatedAt, &w.ObservationVersion, &w.HostID, &w.Disk))
	return
}
func scanOp(r scanner) (o Operation, err error) {
	err = missing(r.Scan(&o.ID, &o.WorkspaceID, &o.Type, &o.Status, &o.Target, &o.Purge, &o.ApplySSH, &o.Phase, &o.Attempts, &o.Error, &o.CreatedAt, &o.FinishedAt, &o.HostID))
	return
}
func scanContainer(r scanner) (c Container, err error) {
	err = missing(r.Scan(&c.Generation, &c.ID, &c.ImageID, &c.Status, &c.CreatedAt, &c.HostID))
	return
}
func findWorkspace(ctx context.Context, q queryer, id string) (Workspace, error) {
	return scanWorkspace(q.QueryRowContext(ctx, "SELECT "+workspaceFields+" FROM workspaces WHERE id=? AND deleted_at IS NULL", id))
}
func (s *Store) Workspace(ctx context.Context, id string) (Workspace, error) {
	return findWorkspace(ctx, s.db, id)
}
func (s *Store) Workspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+workspaceFields+" FROM workspaces WHERE deleted_at IS NULL ORDER BY created_at DESC")
	return collect(rows, err, scanWorkspace)
}
func (s *Store) HostWorkspaces(ctx context.Context, id string) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+workspaceFields+" FROM workspaces WHERE assigned_host_id=? AND deleted_at IS NULL ORDER BY created_at DESC", id)
	return collect(rows, err, scanWorkspace)
}
func (s *Store) Container(ctx context.Context, id string, generation int) (Container, error) {
	return scanContainer(s.db.QueryRowContext(ctx, "SELECT "+containerFields+" FROM containers WHERE workspace_id=? AND generation=?", id, generation))
}
func (s *Store) Containers(ctx context.Context, id string) ([]Container, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+containerFields+" FROM containers WHERE workspace_id=? ORDER BY generation DESC", id)
	return collect(rows, err, scanContainer)
}
func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	return scanOp(s.db.QueryRowContext(ctx, "SELECT "+operationFields+" FROM operations WHERE id=?", id))
}
func (s *Store) Operations(ctx context.Context, id string) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+operationFields+" FROM operations WHERE (?='' OR workspace_id=?) ORDER BY created_at DESC LIMIT 100", id, id)
	return collect(rows, err, scanOp)
}
func (s *Store) HostOperations(ctx context.Context, id string) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+operationFields+" FROM operations WHERE host_id=? ORDER BY created_at DESC LIMIT 100", id)
	return collect(rows, err, scanOp)
}

func existingOperation(ctx context.Context, tx *sql.Tx, key, hash string) (Operation, error) {
	if key == "" {
		return Operation{}, ErrMissing
	}
	var id, previous string
	err := tx.QueryRowContext(ctx, `SELECT id,request_hash FROM operations WHERE idempotency_key=?`, key).Scan(&id, &previous)
	if err != nil {
		return Operation{}, missing(err)
	}
	if previous != hash {
		return Operation{}, ErrConflict
	}
	return scanOp(tx.QueryRowContext(ctx, "SELECT "+operationFields+" FROM operations WHERE id=?", id))
}

func insertOp(ctx context.Context, tx *sql.Tx, w Workspace, kind string, target int, purge bool, key, hash string) (Operation, error) {
	o := Operation{ID: ID(), WorkspaceID: w.ID, HostID: w.HostID, Type: kind, Status: "queued", Target: target, Purge: purge, CreatedAt: now()}
	_, err := tx.ExecContext(ctx, `INSERT INTO operations(id,workspace_id,host_id,type,status,target_generation,purge,idempotency_key,request_hash,created_at)
 VALUES(?,?,?,?,?,?,?,NULLIF(?,''),?,?)`, o.ID, w.ID, w.HostID, kind, o.Status, target, purge, key, hash, o.CreatedAt)
	return o, err
}

func (s *Store) Create(ctx context.Context, w Workspace, key, hash string, sshKeys []string, credential *RepositoryCredential) (o Operation, err error) {
	checkPlacement := w.HostID != ""
	if !checkPlacement {
		w.HostID = LocalHostID
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var e error
		o, e = existingOperation(ctx, tx, key, hash)
		if !errors.Is(e, ErrMissing) {
			return e
		}
		if e = reserveHost(ctx, tx, w, checkPlacement); e != nil {
			return e
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO workspaces(id,name,image,repo_url,branch,cpu_milli,memory_mb,disk_bytes,desired_state,observed_state,assigned_host_id,created_at,updated_at)
  VALUES(?,?,?,?,?,?,?,?,?,'creating',?,?,?) ON CONFLICT(name) WHERE deleted_at IS NULL DO NOTHING`,
			w.ID, w.Name, w.Image, w.RepoURL, w.Branch, w.CPU, w.Memory, w.Disk, w.Desired, w.HostID, now(), now())
		if e = affected(res, e, ErrConflict); e != nil {
			return e
		}
		if e = insertReservation(ctx, tx, w); e != nil {
			return e
		}
		if e = writeSSH(ctx, tx, w.ID, sshKeys); e != nil {
			return e
		}
		if credential != nil {
			if e = writeRepositoryCredential(ctx, tx, w.ID, credential); e != nil {
				return e
			}
		}
		o, e = insertOp(ctx, tx, w, "create", 1, false, key, hash)
		return e
	})
	return
}

var ErrWorkspacePaused = errors.New("Workspace container is paused; unpause it in Docker or stop it before starting")

func (s *Store) Enqueue(ctx context.Context, id, kind string, purge bool, key, hash string) (o Operation, err error) {
	unlock, err := s.admission.lock(ctx, id)
	if err != nil {
		return o, err
	}
	defer unlock()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var e error
		o, e = existingOperation(ctx, tx, key, hash)
		if !errors.Is(e, ErrMissing) {
			return e
		}
		w, e := findWorkspace(ctx, tx, id)
		if e != nil {
			return e
		}
		if kind == "start" && w.Paused {
			return ErrWorkspacePaused
		}
		if e = checkIdle(ctx, tx, id); e != nil {
			return e
		}
		target := w.Generation
		switch kind {
		case "rebuild", "reset":
			target++
		case "start", "stop":
			if target == 0 {
				return ErrBusy
			}
			// A failed replacement or deletion can leave the previous generation unusable.
			if kind == "start" && w.Observed == "error" {
				return ErrBusy
			}
		}
		o, e = insertOp(ctx, tx, w, kind, target, purge, key, hash)
		if e != nil {
			return e
		}
		if kind == "rebuild" || kind == "reset" {
			return reacquireReservation(ctx, tx, w, o)
		}
		return nil
	})
	return
}

func (s *Store) RequeueOperations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET status='queued' WHERE status='running'`)
	return err
}

func (s *Store) ClaimOp(ctx context.Context, requireLocalLease ...bool) (o Operation, w Workspace, err error) {
	localLease := len(requireLocalLease) > 0 && requireLocalLease[0]
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var e error
		o, e = scanOp(tx.QueryRowContext(ctx, `UPDATE operations SET status='running',attempts=attempts+1 WHERE id=(
   SELECT o.id FROM operations o JOIN host_nodes h ON h.id=o.host_id
   WHERE o.status='queued' AND o.retry_after<=unixepoch()
     AND h.scheduling_state <>'disabled'
     AND ((h.id='local' AND NOT ?) OR (h.healthy=1 AND h.last_heartbeat_unix>?))
     AND (SELECT count(*) FROM operations r WHERE r.host_id=o.host_id AND r.status='running') < min(h.max_operations_target,h.max_operations)
   ORDER BY o.created_at,o.id LIMIT 1) RETURNING `+operationFields, localLease, hostCutoff()))
		if e != nil {
			return e
		}
		w, e = findWorkspace(ctx, tx, o.WorkspaceID)
		if e != nil {
			return e
		}
		// A start rejected by preflight must not change the Workspace.
		if o.Type == "start" {
			return nil
		}
		desired, observed := w.Desired, w.Observed
		switch o.Type {
		case "create":
			observed = "creating"
		case "stop":
			desired = "stopped"
		case "rebuild", "reset":
			observed = "rebuilding"
		case "delete":
			desired, observed = "stopped", "deleting"
		}
		w.Desired, w.Observed = desired, observed
		_, e = tx.ExecContext(ctx, `UPDATE workspaces SET desired_state=?,observed_state=?,updated_at=? WHERE id=?`, desired, observed, now(), w.ID)
		return e
	})
	return
}

func (s *Store) OpPhase(ctx context.Context, id string, phase int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET phase=? WHERE id=?`, phase, id)
	return err
}

// An uncertain operation stays exclusive for its Workspace while the same
// generation waits for its Host or is retried after a short delay.
func (s *Store) RetryOp(ctx context.Context, id, message string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE operations SET status='queued',error=?,retry_after=unixepoch()+3 WHERE id=? AND status='running'`, message, id)
	return affected(res, err, ErrBusy)
}

func (s *Store) RejectOp(ctx context.Context, id, message string) error {
	return writeOperationResult(ctx, s.db, id, "failed", message)
}

func writeOperationResult(ctx context.Context, db executor, id, status, message string) error {
	_, err := db.ExecContext(ctx, `UPDATE operations SET status=?,error=?,finished_at=? WHERE id=?`, status, message, now(), id)
	return err
}

func (s *Store) FinishOp(ctx context.Context, o Operation, state string, c *Container, message string) error {
	return s.finishOp(ctx, o, state, c, message, false, false)
}

func (s *Store) FailUnverifiedStart(ctx context.Context, o Operation, message string) error {
	return s.finishOp(ctx, o, "", nil, message, true, false)
}

func (s *Store) FailCleanCreate(ctx context.Context, o Operation, message string) error {
	if o.Type != "create" || message == "" {
		return ErrConflict
	}
	return s.finishOp(ctx, o, "", nil, message, false, true)
}

func (s *Store) finishOp(ctx context.Context, o Operation, state string, c *Container, message string, unverifiedStart, releaseCreate bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		status := "succeeded"
		if message != "" {
			status, state = "failed", "error"
		}
		res, e := tx.ExecContext(ctx, `UPDATE operations SET status=?,error=?,finished_at=? WHERE id=? AND attempts=?
 AND (status='running' OR (status='queued' AND attempts=0))`, status, message, now(), o.ID, o.Attempts)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil || n == 0 {
			return e
		}
		if c != nil && status == "succeeded" {
			if _, e := tx.ExecContext(ctx, `INSERT INTO containers(workspace_id,generation,docker_id,image_id,status,created_at,host_id)
 VALUES(?,?,?,?,?,?,?) ON CONFLICT(workspace_id,generation) DO UPDATE SET
 docker_id=excluded.docker_id,image_id=excluded.image_id,status=excluded.status`, o.WorkspaceID, c.Generation, c.ID, c.ImageID, c.Status, now(), o.HostID); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `UPDATE workspace_reservations SET status='committed',updated_at=? WHERE workspace_id=? AND status='reserved'`, now(), o.WorkspaceID); e != nil {
				return e
			}
		}
		if state == "deleted" || (status == "succeeded" && c != nil) {
			generation := 0
			if c != nil {
				generation = c.Generation
			}
			if _, e := tx.ExecContext(ctx, `UPDATE containers SET status='removed' WHERE workspace_id=? AND generation<>?`, o.WorkspaceID, generation); e != nil {
				return e
			}
		}
		if state == "deleted" {
			if e := writeRepositoryCredential(ctx, tx, o.WorkspaceID, nil); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `DELETE FROM workspace_access WHERE workspace_id=?`, o.WorkspaceID); e != nil {
				return e
			}
		}
		if state == "deleted" || releaseCreate {
			if e := resolveReservation(ctx, tx, o.WorkspaceID, state == "deleted" && !o.Purge); e != nil {
				return e
			}
		}
		var newGen any
		if c != nil && status == "succeeded" {
			newGen = c.Generation
		}
		_, e = tx.ExecContext(ctx, `UPDATE workspaces SET observed_state=?,error=?,generation=COALESCE(?,generation),updated_at=?,
   paused=CASE WHEN ?='succeeded' THEN 0 ELSE paused END, start_unverified=?,
   desired_state=CASE WHEN ?='start' THEN 'running' ELSE desired_state END,
   deleted_at=CASE WHEN ?='deleted' THEN ? ELSE NULL END WHERE id=?`, state, message, newGen, now(), status, unverifiedStart, o.Type, state, now(), o.WorkspaceID)
		return e
	})
}

func (s *Store) CancelOp(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var kind, workspaceID string
		var reacquired bool
		err := tx.QueryRowContext(ctx, `UPDATE operations SET status='cancelled',finished_at=?
 WHERE id=? AND status='queued' AND attempts=0 AND apply_ssh=0 RETURNING type,workspace_id,reacquired_reservation`, now(), id).Scan(&kind, &workspaceID, &reacquired)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBusy
		}
		if err != nil {
			return err
		}
		if kind == "create" || reacquired {
			if err := resolveReservation(ctx, tx, workspaceID, false); err != nil {
				return err
			}
		}
		if kind == "create" {
			if err := writeRepositoryCredential(ctx, tx, workspaceID, nil); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_access WHERE workspace_id=?`, workspaceID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE workspaces SET desired_state='stopped',observed_state='deleted',deleted_at=?,updated_at=? WHERE id=?`, now(), now(), workspaceID)
		}
		return err
	})
}

const PausedObservation = "Docker reports the container is paused."

var ErrObservationConflict = errors.New("workspace observation changed")

func (s *Store) Observe(ctx context.Context, w Workspace, state, message string, paused, startUnverified bool) error {
	unlock, err := s.admission.lock(ctx, w.ID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		changed := state != w.Observed || message != w.Error || paused != w.Paused || startUnverified != w.StartUnverified
		// Even a no-op Docker observation invalidates older samples, without
		// changing the API's updated_at.
		r, e := tx.ExecContext(ctx, `UPDATE workspaces SET observed_state=?,error=?,paused=?,start_unverified=?,
   updated_at=CASE WHEN ? THEN ? ELSE updated_at END, observation_version=observation_version+1
   WHERE id=? AND generation=? AND deleted_at IS NULL AND updated_at=? AND observation_version=?
   AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND status IN ('queued','running'))`,
			state, message, paused, startUnverified, changed, now(), w.ID, w.Generation, w.UpdatedAt, w.ObservationVersion, w.ID)
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			// Retry only a stale snapshot of the same idle generation, not a
			// deletion or a Workspace currently owned by a lifecycle operation.
			var stale bool
			e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces
    WHERE id=? AND generation=? AND deleted_at IS NULL AND (updated_at<>? OR observation_version<>?)
    AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND status IN ('queued','running')))`,
				w.ID, w.Generation, w.UpdatedAt, w.ObservationVersion, w.ID).Scan(&stale)
			if e == nil && stale {
				return ErrObservationConflict
			}
			return e
		}
		if !changed {
			return nil
		}
		if w.StartUnverified && state == "running" && !startUnverified {
			// Record the verified in-place Start without replacing this generation.
			_, e := tx.ExecContext(ctx, `UPDATE containers SET status=? WHERE workspace_id=? AND generation=?`, state, w.ID, w.Generation)
			return e
		}
		return nil
	})
}

func checkIdle(ctx context.Context, tx *sql.Tx, id string) error {
	var occupied bool
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND status IN ('queued','running'))`, id).Scan(&occupied)
	if e != nil {
		return e
	}
	if occupied {
		return ErrBusy
	}
	return nil
}

func (s *Store) ReadyContainer(ctx context.Context, id string) (Workspace, Container, error) {
	var w Workspace
	var c Container
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var e error
		w, e = findWorkspace(ctx, tx, id)
		if e != nil {
			return e
		}
		var busy bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND status IN ('queued','running'))`, id).Scan(&busy)
		if e != nil {
			return e
		}
		if busy || w.Observed != "running" {
			return ErrBusy
		}
		c, e = scanContainer(tx.QueryRowContext(ctx, "SELECT "+containerFields+" FROM containers WHERE workspace_id=? AND generation=?", id, w.Generation))
		return e
	})
	return w, c, err
}

func (s *Store) AdmitTerminal(ctx context.Context, id string, generation int, open func() error) error {
	unlock, err := s.admission.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	w, _, err := s.ReadyContainer(ctx, id)
	if err != nil {
		return err
	}
	if w.Generation != generation {
		return ErrBusy
	}
	return open()
}
