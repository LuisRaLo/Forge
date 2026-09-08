// Package storage implements the domain repositories on top of SQLite.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	// modernc.org/sqlite is a pure-Go SQLite driver. It is chosen over the
	// cgo-based alternative so that ai-squad stays a single statically
	// linked binary that cross-compiles without a C toolchain.
	_ "modernc.org/sqlite"
)

// DB wraps a SQLite handle together with the settings ai-squad relies on.
type DB struct {
	*sql.DB
	path string
}

// Path returns the database file path, or ":memory:" for in-memory databases.
func (d *DB) Path() string { return d.path }

// Open opens (creating if necessary) the SQLite database at path and applies
// the pragmas the orchestrator depends on.
//
// The connection pool is capped at a single connection on purpose. SQLite
// serialises writers anyway, and a single connection removes SQLITE_BUSY and
// lost-update races entirely, which matters more than read parallelism for a
// local orchestrator handling tens of operations per second. Every repository
// method is therefore safe to call from any number of goroutines.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, err
	}

	if path != "" && path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to sqlite database %s: %w", path, err)
	}

	return &DB{DB: db, path: path}, nil
}

// OpenMemory opens a private in-memory database, for tests.
func OpenMemory(ctx context.Context) (*DB, error) {
	return Open(ctx, ":memory:")
}

// buildDSN assembles a modernc.org/sqlite DSN with the required pragmas.
func buildDSN(path string) (string, error) {
	if path == "" {
		path = ":memory:"
	}

	// WAL keeps readers from blocking the writer; busy_timeout covers the
	// brief contention window during checkpointing; foreign_keys enforces
	// the parent-task relation SQLite otherwise ignores.
	pragmas := []string{
		"busy_timeout(10000)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
	}
	if path != ":memory:" {
		pragmas = append(pragmas, "journal_mode(WAL)")
	}

	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	// Take the write lock at BEGIN rather than on first write, so that a
	// read-then-write transaction cannot be upgraded and deadlock.
	q.Set("_txlock", "immediate")

	return "file:" + path + "?" + q.Encode(), nil
}

// withTx runs fn inside a transaction, committing on success and rolling back
// on any error or panic.
func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
