package state

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed schema.sql
var schemaSQL string

// schemaV2SQL adds the budget tables. Tables
// only: no column of schema.sql changes.
//
//go:embed schema_v2.sql
var schemaV2SQL string

// schemaVersion is the user_version schema_v2.sql sets: the newest this gosplan
// writes and reads. schema.sql alone is version 1.
const schemaVersion = 2

// schemaVersionV1 is the v1 schema (schema.sql), migrated on Open and
// still readable by OpenReadOnly.
const schemaVersionV1 = 1

// errReadOnly is returned by write calls on a store from OpenReadOnly.
var errReadOnly = errors.New("state: store is read-only")

// Store is the SQLite state of one state directory.
type Store struct {
	Now func() time.Time // defaults to time.Now; tests replace it

	version int // user_version after Open (always 2) or as found by OpenReadOnly (1 or 2)

	writer   *sql.DB // MaxOpenConns 1; the read-only pool for OpenReadOnly
	path     string
	readOnly bool
	flushMu  sync.Mutex // serialises FlushEvents within this process

	budgetFlushMu sync.Mutex // serialises FlushBudgetEvents within this process
}

// writerDSN is the writer DSN. _txlock=immediate makes every
// BeginTx a BEGIN IMMEDIATE, so a transaction takes the write lock (or
// waits busy_timeout for it) before its first statement.
func writerDSN(path string) string {
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
}

func readOnlyDSN(path string) string {
	return "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
}

// checkPath rejects paths the DSN cannot carry: '?' and '#' end the file
// name of a file: URI and '%' starts an escape.
func checkPath(path string) error {
	if path == "" {
		return errors.New("state: empty database path")
	}
	if strings.ContainsAny(path, "?#%") {
		return fmt.Errorf("state: database path %q contains one of ?#%%", path)
	}
	return nil
}

// Open opens or creates the DB at path with the documented DSN
// (WAL, busy_timeout 5000, foreign_keys, synchronous NORMAL, _txlock=immediate),
// applies schema.sql then schema_v2.sql when user_version is 0, migrates a
// version-1 file to 2 (migrateLeases, then schema_v2.sql), and refuses
// user_version > 2.
func Open(path string) (*Store, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	// 0700 dir, 0600 files: hygiene against other local users only. A
	// worker runs as this same uid and can still read and write all of it
	// (docs/trust-until-d7.md).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	db, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{Now: time.Now, writer: db, path: path, version: schemaVersion}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("state: %w", err)
		}
	}
	return s, nil
}

// init checks the journal mode and applies the schema. The user_version
// check and the schema (or the v1 -> v2 migration: master's migrateLeases,
// then schema_v2.sql, rows untouched) run in one IMMEDIATE transaction, so
// two processes opening a file at once cannot both apply it.
func (s *Store) init(ctx context.Context) error {
	var mode string
	if err := s.writer.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("state: open %s: %w", s.path, err)
	}
	if mode != "wal" {
		return fmt.Errorf("state: open %s: journal_mode is %q, want wal", s.path, mode)
	}
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return fmt.Errorf("state: read user_version: %w", err)
		}
		switch {
		case v == schemaVersion:
			// Idempotent: adds the lease identity columns only if a file
			// lacks them, as master does on every open.
			return migrateLeases(ctx, tx)
		case v == schemaVersionV1:
			if err := migrateLeases(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, schemaV2SQL); err != nil {
				return fmt.Errorf("state: migrate schema 1 -> 2: %w", err)
			}
			return nil
		case v > schemaVersion:
			return fmt.Errorf("state: %s has schema version %d, this gosplan supports up to %d", s.path, v, schemaVersion)
		case v != 0:
			return fmt.Errorf("state: %s has unknown schema version %d", s.path, v)
		}
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("state: apply schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV2SQL); err != nil {
			return fmt.Errorf("state: apply schema v2: %w", err)
		}
		return nil
	})
}

// migrateLeases adds the lease identity columns (nonce, start_time) to a
// database created before them. user_version stays 1: the columns are
// nullable additions and old readers ignore them.
func migrateLeases(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info('leases')`)
	if err != nil {
		return fmt.Errorf("state: migrate leases: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	for _, c := range []string{"nonce TEXT", "start_time INTEGER"} {
		if name, _, _ := strings.Cut(c, " "); !have[name] {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE leases ADD COLUMN `+c); err != nil {
				return fmt.Errorf("state: migrate leases: %w", err)
			}
		}
	}
	return nil
}

// OpenReadOnly opens path with mode=ro for readers such as `gosplan status`.
// Write calls on it return an error. It accepts user_version 1 (a schema v1
// file not yet migrated: no budget tables) and 2; SchemaVersion says which.
func OpenReadOnly(path string) (*Store, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	if v != schemaVersion && v != schemaVersionV1 {
		db.Close()
		return nil, fmt.Errorf("state: %s has schema version %d, want %d or %d", path, v, schemaVersionV1, schemaVersion)
	}
	return &Store{Now: time.Now, writer: db, path: path, readOnly: true, version: v}, nil
}

// SchemaVersion is the database's user_version: 2 for a store from Open,
// 1 or 2 for one from OpenReadOnly. Version 1 has no budget tables.
func (s *Store) SchemaVersion() int { return s.version }

// IsBusy reports whether err is SQLITE_BUSY or SQLITE_LOCKED: another
// connection holds a lock this one waited busy_timeout for.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	var c interface{ Code() int }
	if errors.As(err, &c) {
		switch c.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// Close closes the database.
func (s *Store) Close() error { return s.writer.Close() }

// DB returns the underlying pool, for read queries in other state files and
// tests. The writer pool has one connection: do not use DB() inside an
// Immediate fn (use its tx), or the call waits forever for the connection.
func (s *Store) DB() *sql.DB { return s.writer }

// Immediate runs fn in one BEGIN IMMEDIATE transaction. It commits if fn
// returns nil and rolls back otherwise, returning fn's error unchanged
// (so errors.Is(err, core.ErrConflict) works).
func (s *Store) Immediate(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.readOnly {
		return errReadOnly
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit: %w", err)
	}
	return nil
}
