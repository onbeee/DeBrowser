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
	"net/url"
	"os"
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
// only one writer at a time. A single connection also guarantees the
// per-connection pragmas (foreign_keys, synchronous) apply to every
// statement, which a multi-connection pool could not promise.
type DB struct {
	sql       *sql.DB
	path      string
	backupDir string
}

// Options configures optional database behaviour.
type Options struct {
	// BackupDir, when set, receives a verified pre-migration backup
	// (see Migrate) before any pending migration is applied to an
	// existing database. In production this is the portable layout's
	// backups directory.
	BackupDir string
}

// Open opens (creating when needed) the SQLite database at path and applies
// the connection pragmas required by AGENTS.md: WAL mode and foreign key
// enforcement.
func Open(path string) (*DB, error) {
	return OpenWithOptions(path, Options{})
}

// OpenWithOptions is Open with explicit options.
//
// Pragmas, and why:
//   - journal_mode=WAL: readers do not block the single writer and the
//     database survives application crashes.
//   - synchronous=FULL: with WAL, NORMAL only guarantees durability
//     across application crashes; FULL also survives OS crashes and
//     power loss for committed transactions. DeBrowser is a local-first
//     metadata store with a low write rate, so the small extra commit
//     latency is the right trade for never losing committed instance,
//     proxy or bookmark data (AGENTS.md prioritises data integrity).
//   - foreign_keys=1: SQLite disables FK enforcement by default; the
//     schema's referential integrity depends on it.
//   - busy_timeout=5000: brief lock contention (for example a backup
//     snapshot) waits instead of failing with SQLITE_BUSY.
func OpenWithOptions(path string, opts Options) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("database: resolve %q: %w", path, err)
	}
	dsn := sqliteFileURI(abs) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
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
	return &DB{sql: sqlDB, path: abs, backupDir: opts.BackupDir}, nil
}

// sqliteFileURI builds a SQLite file URI for path with the path
// percent-encoded. Concatenating "file:" + path directly would let
// URI-special characters in a directory name corrupt the target: a
// '#' would start a fragment and silently open a different (truncated)
// file, a '?' would start a query, and a '%' would be misread as an
// escape. A portable application folder can legitimately contain any
// of these, so the path always goes through net/url encoding. Windows
// drive paths (C:\...) and UNC paths (\\server\share\...) are handled.
func sqliteFileURI(path string) string {
	slash := filepath.ToSlash(path)
	u := url.URL{Scheme: "file"}
	switch {
	case strings.HasPrefix(slash, "//"):
		// UNC path: //server/share/... — the server is the URI host.
		host, tail, _ := strings.Cut(strings.TrimPrefix(slash, "//"), "/")
		u.Host = host
		u.Path = "/" + tail
	case strings.HasPrefix(slash, "/"):
		u.Path = slash
	default:
		// Windows drive path (C:/...).
		u.Path = "/" + slash
	}
	return u.String()
}

// Path returns the database file location (from internal/paths in
// production).
func (db *DB) Path() string { return db.path }

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

// WithTx runs fn inside a single transaction and commits it when fn
// returns nil; any error rolls the whole unit of work back. It is the
// shared transaction entry point for cross-repository operations.
func (db *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit transaction: %w", err)
	}
	return nil
}

// Migrate applies every not-yet-applied migration in version order. Each
// migration runs in its own transaction together with its bookkeeping row,
// so a failed migration never leaves the schema half-applied.
//
// When a backup directory is configured (production) and the database
// already holds applied migrations, a verified backup of the current
// state is taken BEFORE the first pending migration runs. The backup
// uses VACUUM INTO, which captures WAL-resident data consistently —
// copying the .db file alone would silently lose uncheckpointed
// transactions. If the backup fails, no migration is attempted.
func (db *DB) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	return db.applyMigrations(ctx, migrations)
}

// pendingMigrations returns the subset of migrations not yet recorded
// in schema_migrations, preserving version order.
func (db *DB) pendingMigrations(ctx context.Context, migrations []migration) ([]migration, int, error) {
	var applied int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&applied); err != nil {
		return nil, 0, fmt.Errorf("database: count applied migrations: %w", err)
	}
	var pending []migration
	for _, m := range migrations {
		var count int
		if err := db.sql.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&count); err != nil {
			return nil, 0, fmt.Errorf("database: check migration %d: %w", m.version, err)
		}
		if count == 0 {
			pending = append(pending, m)
		}
	}
	return pending, applied, nil
}

func (db *DB) applyMigrations(ctx context.Context, migrations []migration) error {
	if _, err := db.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("database: create schema_migrations: %w", err)
	}
	pending, applied, err := db.pendingMigrations(ctx, migrations)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	// Protect existing data before changing the schema. A brand-new
	// database (nothing applied yet) has nothing to protect.
	if applied > 0 && db.backupDir != "" {
		if err := db.backupBeforeMigration(ctx, migrations[len(migrations)-1].version); err != nil {
			return err
		}
	}
	for _, m := range pending {
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

// AppliedVersion returns the highest applied migration version, or 0
// for a database that has not been migrated yet.
func (db *DB) AppliedVersion(ctx context.Context) (int, error) {
	var version sql.NullInt64
	err := db.sql.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		// schema_migrations may not exist yet on a brand-new file.
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("database: applied version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// backupBeforeMigration snapshots the current database into the backup
// directory and verifies the snapshot before any schema change runs.
// The snapshot uses VACUUM INTO (see snapshotTo), so data still living
// in the WAL is included. Any failure aborts the migration.
func (db *DB) backupBeforeMigration(ctx context.Context, toVersion int) error {
	current, err := db.AppliedVersion(ctx)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	name := fmt.Sprintf("debrowser-pre-migration-%s-from-v%d-to-v%d.db", stamp, current, toVersion)
	target := filepath.Join(db.backupDir, name)
	if err := db.snapshotTo(ctx, target); err != nil {
		return fmt.Errorf("database: pre-migration backup to %s failed, migration aborted: %w", target, err)
	}
	return nil
}

// snapshotTo writes a transactionally consistent copy of the database
// to target (which must not exist yet) using VACUUM INTO and verifies
// it with integrity_check. It is the only supported way to copy the
// live database: copying the .db file directly would miss data that
// still lives in the WAL.
func (db *DB) snapshotTo(ctx context.Context, target string) error {
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("database: backup target already exists: %s", target)
	}
	escaped := strings.ReplaceAll(filepath.ToSlash(target), "'", "''")
	if _, err := db.sql.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("database: snapshot: %w", err)
	}
	check, err := sql.Open("sqlite", sqliteFileURI(target)+"?mode=ro")
	if err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("database: open backup for verification: %w", err)
	}
	defer func() { _ = check.Close() }()
	var integrity string
	if err := check.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("database: verify backup: %w", err)
	}
	if integrity != "ok" {
		_ = os.Remove(target)
		return fmt.Errorf("database: backup failed integrity_check: %s", integrity)
	}
	return nil
}

// BackupNow writes a verified snapshot of the current database into
// the configured backup directory and returns its path. It gives later
// stages (manual backups, Stage 7) the same WAL-safe mechanism the
// migration path uses.
func (db *DB) BackupNow(ctx context.Context) (string, error) {
	if db.backupDir == "" {
		return "", fmt.Errorf("database: no backup directory configured")
	}
	version, err := db.AppliedVersion(ctx)
	if err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102-150405.000")
	target := filepath.Join(db.backupDir, fmt.Sprintf("debrowser-backup-%s-v%d.db", stamp, version))
	if err := db.snapshotTo(ctx, target); err != nil {
		return "", err
	}
	return target, nil
}
