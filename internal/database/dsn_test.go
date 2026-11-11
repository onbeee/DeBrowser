package database

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The file URI must percent-encode URI-special characters so a folder
// named e.g. "DeBrowser #2" cannot redirect the database to a
// truncated or different path.
func TestFileURIEncodesSpecialCharacters(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"hash", "/data/app #2/debrowser.db"},
		{"question", "/data/app?x/debrowser.db"},
		{"percent", "/data/100%/debrowser.db"},
		{"spaces and non-ascii", "/data/数据 目录/debrowser.db"},
		{"windows drive", "C:/DeBrowser #1/data/debrowser.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uri := FileURI(tc.path)
			parsed, err := url.Parse(uri)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", uri, err)
			}
			if parsed.Scheme != "file" {
				t.Errorf("scheme = %q, want file", parsed.Scheme)
			}
			want := tc.path
			if !strings.HasPrefix(want, "/") {
				want = "/" + want // Windows drive gains the URI leading slash.
			}
			if parsed.Path != want {
				t.Errorf("decoded path = %q, want %q (uri %q)", parsed.Path, want, uri)
			}
			if strings.ContainsAny(parsed.EscapedPath(), "#?") {
				t.Errorf("escaped path still contains a raw special character: %q", uri)
			}
		})
	}
}

// End to end: a database inside directories whose names contain URI
// special characters must be created, written, closed and reopened at
// exactly the intended file — never at a truncated sibling path.
func TestOpenDatabaseUnderSpecialCharDirectories(t *testing.T) {
	names := []string{"app#2", "app%20x", "app dir", "数据#1"}
	if runtime.GOOS != "windows" {
		// '?' is legal on POSIX filesystems but illegal in Windows
		// file names, so it can only be exercised end-to-end here.
		names = append(names, "app?x")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			path := filepath.Join(dir, "debrowser.db")
			ctx := context.Background()

			db, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := db.Migrate(ctx); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if _, err := db.SQL().ExecContext(ctx,
				`INSERT INTO app_settings (key, value) VALUES ('k', 'special')`); err != nil {
				t.Fatalf("insert: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The database file must exist at the exact intended path...
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database file missing at intended path: %v", err)
			}
			// ...and nothing may have appeared at a truncated path
			// (e.g. base/app) or anywhere else unexpected.
			entries, err := os.ReadDir(base)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != name {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("parent dir contains %v, want only %q", names, name)
			}

			// Reopening reads back the same database.
			db2, err := Open(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = db2.Close() }()
			var v string
			if err := db2.SQL().QueryRowContext(ctx,
				`SELECT value FROM app_settings WHERE key='k'`).Scan(&v); err != nil {
				t.Fatalf("read after reopen: %v", err)
			}
			if v != "special" {
				t.Errorf("value after reopen = %q, want special", v)
			}
		})
	}
}

// UNC paths: \\server\share\... must become file://server/share/...
// with the server as the URI host. On Windows, filepath.ToSlash turns
// the backslash form into the //server/share form first, so the test
// exercises that canonical form on every platform (plus the raw
// backslash form on Windows itself).
func TestFileURIUNC(t *testing.T) {
	uri := FileURI("//server/share/DeBrowser/data/debrowser.db")
	if uri != "file://server/share/DeBrowser/data/debrowser.db" {
		t.Errorf("FileURI(UNC) = %q, want file://server/share/DeBrowser/data/debrowser.db", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", uri, err)
	}
	if parsed.Host != "server" || parsed.Path != "/share/DeBrowser/data/debrowser.db" {
		t.Errorf("parsed UNC = host %q path %q, want server + /share/DeBrowser/data/debrowser.db",
			parsed.Host, parsed.Path)
	}
	if runtime.GOOS == "windows" {
		if got := FileURI(`\\server\share\DeBrowser\data\debrowser.db`); got != uri {
			t.Errorf("FileURI(backslash UNC) = %q, want %q", got, uri)
		}
	}
}
