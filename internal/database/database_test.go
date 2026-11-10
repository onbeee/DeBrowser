package database

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenEnablesWALAndForeignKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var journalMode string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode error = %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want %q", journalMode, "wal")
	}

	var foreignKeys int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys error = %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}

	var applied int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations error = %v", err)
	}
	if applied != 1 {
		t.Errorf("applied migrations = %d, want 1", applied)
	}

	// The foundation table must exist and accept rows.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO app_settings (key, value) VALUES ('schema_smoke', 'ok')`); err != nil {
		t.Fatalf("insert into app_settings error = %v", err)
	}
}

func TestHealth(t *testing.T) {
	db := openTestDB(t)
	if err := db.Health(context.Background()); err != nil {
		t.Errorf("Health() error = %v", err)
	}
}
