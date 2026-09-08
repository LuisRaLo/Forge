package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"

	"github.com/santillana/ai-squad/migrations"
)

// migrationsTable records which migrations have been applied.
const migrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);`

// Migrate applies every pending migration from the embedded filesystem. It is
// idempotent: running it repeatedly is a no-op, which is what lets `ai-squad
// init` be safely re-run and lets the daemon self-heal on start.
//
// Applied migrations are checksummed. If a migration file changes after it has
// been applied, Migrate fails loudly rather than leaving the schema in a state
// nobody can reason about.
func Migrate(ctx context.Context, db *DB) error {
	return MigrateFS(ctx, db, migrations.FS)
}

// MigrateFS applies migrations from an arbitrary filesystem, for tests.
func MigrateFS(ctx context.Context, db *DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, migrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(files)

	applied, err := appliedMigrations(ctx, db.DB)
	if err != nil {
		return err
	}

	for _, name := range files {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])

		if existing, ok := applied[name]; ok {
			if existing != checksum {
				return fmt.Errorf(
					"migration %s has changed since it was applied (recorded %s, found %s); "+
						"add a new migration instead of editing an applied one",
					name, existing[:12], checksum[:12])
			}
			continue
		}

		if err := applyMigration(ctx, db.DB, name, checksum, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, name, checksum, body string) error {
	err := withTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, body); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES (?, ?)`,
			name, checksum)
		if err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		return nil
	})
	return err
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		out[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return out, nil
}
