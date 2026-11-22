package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 9A: migration 0011 added external_browsers (registrations
// of browsers the user added by hand); migration 0012 then
// reshaped it into a pure path registration table. This test
// covers the FINAL shape; stage12_test.go covers the 0011→0012
// upgrade itself. DeBrowser never auto-detects browsers; this
// table is the only browser reference the product persists
// besides managed kernels.

func TestMigration11FinalShape(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 17 {
		t.Fatalf("AppliedVersion = %d (err %v), want 12", v, err)
	}
	var name string
	err = db.SQL().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='external_browsers'`).Scan(&name)
	if err != nil || name != "external_browsers" {
		t.Fatalf("external_browsers table missing: %v", err)
	}
	// A pure path registration: any display name and any path can
	// be stored — there is no browser-type or verification column
	// left to satisfy.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO external_browsers (id, name, executable_path)
		 VALUES ('b1', 'my-tool', 'C:\tools\my-tool.exe')`); err != nil {
		t.Fatalf("insert manual browser: %v", err)
	}
	// The old verification columns are gone.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO external_browsers (id, name, browser_type, executable_path)
		 VALUES ('b2', 'Other', 'firefox', 'C:\firefox.exe')`); err == nil {
		t.Fatal("browser_type column must not exist after migration 0012")
	}
	// One registration per executable path.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO external_browsers (id, name, executable_path)
		 VALUES ('b3', 'dupe', 'C:\tools\my-tool.exe')`); err == nil {
		t.Fatal("duplicate executable_path must fail")
	}
	// Historical data coexists untouched.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO kernels (id, version, install_path, executable_path) VALUES ('k1', '142.0.1.2', 'kernels/142.0.1.2', 'kernels/142.0.1.2/chrome.exe')`); err != nil {
		t.Fatalf("kernel insert beside external browsers: %v", err)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
}
