package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrSetupCompleted  = errors.New("administrator already exists")
	ErrInvalidPassword = errors.New("incorrect current password")
)

func (s *Store) SetupRequired(ctx context.Context) (bool, error) {
	var required bool
	err := s.db.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM users)`).Scan(&required)
	return required, err
}

func (s *Store) CreateAdmin(ctx context.Context, user, password, digest string, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrSetupCompleted
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users VALUES(?,?)`, user, string(hash)); err != nil {
			return err
		}
		return insertSession(ctx, tx, digest, user, expires)
	})
}

func (s *Store) PasswordHash(ctx context.Context) (user, hash string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT username,password_hash FROM users LIMIT 1`).Scan(&user, &hash)
	return user, hash, missing(err)
}

func (s *Store) NewSession(ctx context.Context, digest, user, hash string, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, time.Now().Unix()); err != nil {
			return err
		}
		// Credentials may have changed while login was checking the password.
		result, err := tx.ExecContext(ctx, `INSERT INTO sessions SELECT ?,username,? FROM users WHERE username=? AND password_hash=?`, digest, expires.Unix(), user, hash)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count == 0 {
			return ErrMissing
		}
		return err
	})
}

func (s *Store) UpdateAdmin(ctx context.Context, currentDigest, currentPassword, user, newPassword, digest string, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var oldUser, hash string
		err := tx.QueryRowContext(ctx, `SELECT u.username,u.password_hash FROM users u
			JOIN sessions s ON s.username=u.username WHERE s.token_hash=? AND s.expires_at>?`, currentDigest, time.Now().Unix()).Scan(&oldUser, &hash)
		if err != nil {
			return missing(err)
		}
		if len(currentPassword) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(currentPassword)) != nil {
			return ErrInvalidPassword
		}
		if newPassword != "" {
			updated, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			hash = string(updated)
		}
		// The username foreign key has no ON UPDATE CASCADE.
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET username=?,password_hash=? WHERE username=?`, user, hash, oldUser); err != nil {
			return err
		}
		return insertSession(ctx, tx, digest, user, expires)
	})
}

func insertSession(ctx context.Context, tx *sql.Tx, digest, user string, expires time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions VALUES(?,?,?)`, digest, user, expires.Unix())
	return err
}

func ResetAdmin(ctx context.Context, path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Open the existing database directly so that reset only changes authentication data.
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=rw&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DELETE FROM users`)
	return err
}

func (s *Store) Session(ctx context.Context, digest string) (string, error) {
	var user string
	err := s.db.QueryRowContext(ctx, `SELECT username FROM sessions WHERE token_hash=? AND expires_at>?`, digest, time.Now().Unix()).Scan(&user)
	return user, missing(err)
}

func (s *Store) EndSession(ctx context.Context, digest string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, digest)
	return err
}
