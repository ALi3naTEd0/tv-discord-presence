// Package store keeps Couchlog's history in SQLite: users, devices,
// sessions, items and their deliveries to Trakt and Simkl.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: cross-compiles without CGO
)

//go:embed schema.sql
var schemaV1 string

//go:embed schema_v2.sql
var schemaV2 string

//go:embed schema_v3.sql
var schemaV3 string

// migrations[i] takes the database from version i to i+1.
var migrations = []string{schemaV1, schemaV2, schemaV3}

// LocalUserID is the single user of a self-hosted install.
const LocalUserID int64 = 1

// Store is an open Couchlog database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and brings its schema
// up to date. It refuses a database written by a newer Couchlog.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	// The path goes in a file: URI, so characters that mean something there
	// (% ? #) are escaped; Windows profile folders can contain them.
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(filepath.ToSlash(path))
	dsn := "file:" + esc + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	// One writer at a time is what SQLite allows anyway; a single connection
	// also keeps the per-connection pragmas in force.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.ensureLocalUser(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that build their own queries in this module.
func (s *Store) DB() *sql.DB { return s.db }

// ErrTooNew means the database was created by a newer Couchlog.
var ErrTooNew = errors.New("store: this database is from a newer Couchlog; update Couchlog")

// SchemaVersion returns the database's schema version (SQLite user_version).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

func (s *Store) migrate(ctx context.Context) error {
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if v > len(migrations) {
		return ErrTooNew
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration to version %d: %w", v+1, err)
		}
		// PRAGMA does not take parameters; v is an int we control.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureLocalUser(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO users(id, name, created_at) VALUES (?, 'local', ?)",
		LocalUserID, ms(time.Now()))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }
