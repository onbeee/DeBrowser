// Package database owns DeBrowser's SQLite database: opening the file with
// the required pragmas, running migrations transactionally and reporting
// health. Browser cookies and browser profile contents are never stored
// in this database (see AGENTS.md section 5).
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver (CGO-free builds).
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps the SQLite connection pool. Access is safe for concurrent use;
// the pool is limited to a single open connection because SQLite allows
// only one writer at a time.
type DB struct {
	sql *sql.DB
}

// Open opens (creating when needed) the SQLite database at path and applies
// the connection pragmas required by AGENTS.md: WAL mode and foreign key
// enforcement.
func Open(path string) (*DB, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("database: open %q: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("database: ping %q: %w", path, err)
	}
	return &DB{sql: sqlDB}, nil
}

// SQL exposes the underlying handle for the business modules built on top
// of this foundation in later stages.
func (db *DB) SQL() *sql.DB { return db.sql }

// Health reports whether the database answers a ping.
func (db *DB) Health(ctx context.Context) error {
	if err := db.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("database: health check: %w", err)
	}
	return nil
}

// Close closes the database.
func (db *DB) Close() error {
	if err := db.sql.Close(); err != nil {
		return fmt.Errorf("database: close: %w", err)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("database: read migrations: %w", err)
	}
	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		versionPart, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("database: migration %q does not start with a version prefix", entry.Name())
		}
		version, err := strconv.Atoi(versionPart)
		if err != nil {
			return nil, fmt.Errorf("database: migration %q has an invalid version prefix: %w", entry.Name(), err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("database: read migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: entry.Name(), sql: string(body)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// Migrate applies every not-yet-applied migration in version order. Each
// migration runs in its own transaction together with its bookkeeping row,
// so a failed migration never leaves the schema half-applied.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("database: create schema_migrations: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migrations {
		var count int
		if err := db.sql.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&count); err != nil {
			return fmt.Errorf("database: check migration %d: %w", m.version, err)
		}
		if count > 0 {
			continue
		}
		tx, err := db.sql.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("database: begin migration %d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("database: apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("database: record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("database: commit migration %d: %w", m.version, err)
		}
	}
	return nil
}
