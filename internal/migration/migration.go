// Package migration moves Stage 1 data from the legacy roaming
// location (%APPDATA%\DeBrowser) into the portable layout the first
// time the portable build starts.
//
// Safety rules (data integrity first):
//   - Copy first, verify, never move: the legacy directory is left
//     fully intact as a backup and is never deleted by this package.
//   - The SQLite database is copied with VACUUM INTO from a read-only
//     connection, which produces a transactionally consistent snapshot
//     even when the legacy database is in WAL mode with a live -wal
//     file. The copy is verified with PRAGMA integrity_check and a
//     schema comparison before it is accepted.
//   - Existing target files are never overwritten. If the portable
//     database already exists, migration is a no-op (idempotent).
//   - If the database copy fails, no partial target database is left
//     behind and startup must abort with the returned error instead of
//     silently creating a second, empty database.
//   - WebView2 data (%APPDATA%\debrowser.exe\EBWebView) is never
//     migrated or mixed with business data. The portable build starts
//     WebView2 with a fresh user data folder under the application
//     root (safe re-initialization); the legacy folder stays untouched.
package migration

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/paths"
)

// Report describes what a migration run did, for logs and for the
// migration-report.json written into the portable data directory.
type Report struct {
	Needed         bool      `json:"needed"`
	DatabaseCopied bool      `json:"databaseCopied"`
	FilesCopied    int       `json:"filesCopied"`
	FilesSkipped   int       `json:"filesSkipped"`
	LegacyRoot     string    `json:"legacyRoot"`
	LegacyWebView2 bool      `json:"legacyWebView2Present"`
	Note           string    `json:"note,omitempty"`
	At             time.Time `json:"at"`
}

// DefaultLegacyRoot returns the Stage 1 data root:
// <os user config dir>/DeBrowser (on Windows, %APPDATA%\DeBrowser).
func DefaultLegacyRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("legacy data location: %w", err)
	}
	return filepath.Join(dir, config.LegacyDirName), nil
}

// Run migrates legacy data into layout when needed. layout.Ensure must
// already have been called. legacyRoot may be "" to skip detection
// (used on non-first runs once the portable database exists).
func Run(layout paths.Layout, legacyRoot string) (Report, error) {
	report := Report{LegacyRoot: legacyRoot, At: time.Now().UTC()}
	if legacyRoot == "" {
		report.Note = "no legacy location"
		return report, nil
	}
	if _, err := os.Stat(legacyRoot); os.IsNotExist(err) {
		report.Note = "legacy directory not present (fresh install)"
		return report, nil
	}
	// A legacy WebView2 folder next to the legacy root is noted for the
	// record only; it is never read, copied or deleted.
	if _, err := os.Stat(filepath.Join(filepath.Dir(legacyRoot), "debrowser.exe", "EBWebView")); err == nil {
		report.LegacyWebView2 = true
	}

	legacyDB := filepath.Join(legacyRoot, "debrowser.db")
	_, legacyDBErr := os.Stat(legacyDB)
	_, targetErr := os.Stat(layout.DatabaseFile)
	if targetErr == nil {
		report.Note = "portable database already exists; nothing overwritten"
		return report, nil
	}
	if os.IsNotExist(legacyDBErr) && !hasFiles(legacyRoot) {
		report.Note = "legacy directory holds no data"
		return report, nil
	}

	report.Needed = true
	if legacyDBErr == nil {
		if err := copyDatabase(legacyDB, layout.DatabaseFile); err != nil {
			return report, err
		}
		report.DatabaseCopied = true
	}
	for _, pair := range [][2]string{
		{filepath.Join(legacyRoot, "backups"), layout.Backups},
		{filepath.Join(legacyRoot, "profiles"), layout.Profiles},
	} {
		copied, skipped, err := copyTree(pair[0], pair[1])
		if err != nil {
			return report, err
		}
		report.FilesCopied += copied
		report.FilesSkipped += skipped
	}
	report.Note = "legacy directory kept intact as backup; WebView2 data re-initialized under the application root"
	if err := writeReport(layout, report); err != nil {
		return report, err
	}
	return report, nil
}

func hasFiles(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = true
		}
		return nil
	})
	return found
}

// copyDatabase snapshots the legacy database into target with
// VACUUM INTO and verifies the result. On any failure the partial
// target is removed and the legacy database is left untouched.
func copyDatabase(legacyDB, target string) error {
	dsn := database.FileURI(legacyDB) + "?mode=ro&_pragma=busy_timeout(5000)"
	src, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open legacy database: %w", err)
	}
	defer func() { _ = src.Close() }()
	src.SetMaxOpenConns(1)

	escaped := strings.ReplaceAll(filepath.ToSlash(target), "'", "''")
	if _, err := src.Exec("VACUUM INTO '" + escaped + "'"); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("copy legacy database (is the old DeBrowser still running? close it and restart): %w", err)
	}
	if err := verifyDatabase(src, target); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}

func verifyDatabase(src *sql.DB, target string) error {
	dst, err := sql.Open("sqlite", database.FileURI(target)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open copied database: %w", err)
	}
	defer func() { _ = dst.Close() }()
	dst.SetMaxOpenConns(1)

	var integrity string
	if err := dst.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("verify copied database: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("copied database failed integrity_check: %s", integrity)
	}
	srcTables, err := tableNames(src)
	if err != nil {
		return fmt.Errorf("read legacy schema: %w", err)
	}
	dstTables, err := tableNames(dst)
	if err != nil {
		return fmt.Errorf("read copied schema: %w", err)
	}
	if strings.Join(srcTables, ",") != strings.Join(dstTables, ",") {
		return fmt.Errorf("copied database schema mismatch: legacy %v, copy %v", srcTables, dstTables)
	}
	return nil
}

func tableNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// copyTree copies files from src to dst recursively. Existing target
// files are never overwritten (skipped and counted). Every copied file
// is SHA-256 verified against its source.
func copyTree(src, dst string) (copied, skipped int, err error) {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return 0, 0, nil
	}
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if _, err := os.Stat(target); err == nil {
			skipped++
			return nil
		}
		if err := copyFileVerified(path, target); err != nil {
			return err
		}
		copied++
		return nil
	})
	if err != nil {
		return copied, skipped, fmt.Errorf("copy %s: %w", src, err)
	}
	return copied, skipped, nil
}

func copyFileVerified(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	sum, err := hashFile(src)
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		_ = os.Remove(dst)
		return fmt.Errorf("copied file failed verification: %s", dst)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeReport(layout paths.Layout, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode migration report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layout.Data, "migration-report.json"), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write migration report: %w", err)
	}
	return nil
}
