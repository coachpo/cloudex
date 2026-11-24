package store

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
)

// The sealed record binds the credentials to the destination and pinned key.
// Host listings only read the separately stored public connection details.
func sealSSHConfig(id string, cfg hostssh.Config, key cipher.AEAD) ([]byte, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, key.NonceSize())
	rand.Read(nonce)
	return key.Seal(nonce, nonce, body, []byte("cloudex:ssh-host:"+id)), nil
}

func (s *Store) AddSSHHost(ctx context.Context, name string, cfg hostssh.Config, key cipher.AEAD) (h Host, err error) {
	id := ID()
	sealed, err := sealSSHConfig(id, cfg, key)
	if err != nil {
		return h, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO host_nodes
			(id,name,created_at,connection_type,ssh_address,ssh_user,ssh_host_key,ssh_config,ssh_revision)
			VALUES(?,?,?,'ssh',?,?,?,?,1) ON CONFLICT(name) DO NOTHING`,
			id, name, now(), cfg.Address, cfg.User, cfg.HostKey, sealed)
		if err := affected(res, err, ErrConflict); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO host_capacity(host_id) VALUES(?)", id); err != nil {
			return err
		}
		h, err = findHost(ctx, tx, id)
		return err
	})
	return
}

func (s *Store) UpdateSSHHost(ctx context.Context, id string, cfg hostssh.Config, key cipher.AEAD) (h Host, err error) {
	sealed, err := sealSSHConfig(id, cfg, key)
	if err != nil {
		return h, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		h, err = findHost(ctx, tx, id)
		if err != nil {
			return err
		}
		if h.ConnectionType != "ssh" || h.SSHAddress != cfg.Address || h.SSHUser != cfg.User {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE host_nodes SET ssh_host_key=?,ssh_config=?,ssh_revision=ssh_revision+1,
			last_heartbeat_unix=0,connection_error='' WHERE id=?`, cfg.HostKey, sealed, id)
		if err != nil {
			return err
		}
		h, err = findHost(ctx, tx, id)
		return err
	})
	return
}

func (s *Store) SSHConfig(ctx context.Context, id string, key cipher.AEAD) (hostssh.Config, error) {
	var cfg hostssh.Config
	var sealed []byte
	if err := s.db.QueryRowContext(ctx, `SELECT ssh_config FROM host_nodes WHERE id=? AND connection_type='ssh'`, id).Scan(&sealed); err != nil {
		return cfg, missing(err)
	}
	n := key.NonceSize()
	if len(sealed) < n {
		return cfg, errors.New("invalid SSH Host credential record")
	}
	clear, err := key.Open(nil, sealed[:n], sealed[n:], []byte("cloudex:ssh-host:"+id))
	if err != nil {
		return cfg, errors.New("cannot decrypt SSH Host credentials with CLOUDEX_MASTER_KEY")
	}
	err = json.Unmarshal(clear, &cfg)
	return cfg, err
}

func (s *Store) SSHHeartbeat(ctx context.Context, id string, revision int64, report HostReport) error {
	return s.heartbeat(ctx, id, "connection_type='ssh' AND ssh_revision=?", revision, report)
}

// SSHDegraded records a responsive SSH connection whose Docker inventory is
// unavailable. Keep the last snapshot visible while suspending placement and dispatch.
func (s *Store) SSHDegraded(ctx context.Context, id string, revision int64, message string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE host_nodes SET healthy=0,last_heartbeat_unix=unixepoch(),last_seen=?,connection_error=?
		WHERE id=? AND connection_type='ssh' AND ssh_revision=? AND scheduling_state<>'disabled'`, now(), message, id, revision)
	return affected(res, err, ErrHostUnauthorized)
}

// SSHFailure keeps the last inventory visible while making the host ineligible
// for dispatch. A disconnected session cannot revoke its replacement's lease.
func (s *Store) SSHFailure(ctx context.Context, id string, revision int64, message string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE host_nodes SET last_heartbeat_unix=0,connection_error=?
		WHERE id=? AND connection_type='ssh' AND ssh_revision=? AND scheduling_state<>'disabled'`, message, id, revision)
	return affected(res, err, ErrHostUnauthorized)
}
