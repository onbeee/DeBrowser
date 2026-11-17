package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 9C: migration 0014 adds the bookmark library tree
// (bookmark_nodes). The historical flat bookmarks table of
// migration 0003 and its rows must survive untouched.

func TestMigration14AddsBookmarkLibraryPreservingLegacyBookmarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 13; v++ {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, 'historical')`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE bookmarks (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL,
		instance_id TEXT, sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO bookmarks (id, name, url) VALUES ('b1', '旧书签', 'https://old.example/')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 14 {
		t.Fatalf("AppliedVersion = %d (err %v), want 14", v, err)
	}
	var legacy string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT url FROM bookmarks WHERE id = 'b1'`).Scan(&legacy); err != nil || legacy != "https://old.example/" {
		t.Fatalf("legacy bookmark lost: %q, %v", legacy, err)
	}
	// The new tree accepts nested nodes.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO bookmark_nodes (id, kind, name) VALUES ('f1', 'folder', '文件夹')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO bookmark_nodes (id, kind, name, url, parent_id) VALUES ('n1', 'bookmark', '书签', 'https://a.example/', 'f1')`); err != nil {
		t.Fatal(err)
	}
	// Kind is constrained.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO bookmark_nodes (id, kind, name) VALUES ('x1', 'other', 'bad')`); err == nil {
		t.Error("invalid kind accepted")
	}
}
