package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrMissing  = errors.New("not found")
	ErrBusy     = errors.New("workspace has active work or is not ready")
	ErrConflict = errors.New("request conflicts with existing data")
)

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type Store struct {
	db           *sql.DB
	admission    workspaceGates
	sshAdmission sync.RWMutex
	sshKeySync   workspaceGates
}

func ID() string { var b [16]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

func Open(ctx context.Context, path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	source, err := fs.Sub(migrations, "migrations")
	if err != nil {
		db.Close()
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, source, goose.WithDisableGlobalRegistry(true))
	if err == nil {
		_, err = provider.Up(ctx)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit()
}

func (s *Store) Instance(ctx context.Context) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta VALUES('instance',?)`, ID()); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance'`).Scan(&id)
	return id, err
}

type scanner interface{ Scan(...any) error }
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMissing
	}
	return err
}

func collect[T any](rows *sql.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
