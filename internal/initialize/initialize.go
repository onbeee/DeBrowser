// Package initialize implements the Stage 5A new-user initialization:
// the one path that turns an exe-only portable folder into an
// initialized installation, reusing the exact building blocks of the
// normal startup (paths layout, database open + migrations, vault
// open, default settings) instead of duplicating them.
//
// Protocol (instruction §6):
//  1. snapshot what already exists under the formal directories
//  2. record the start of initialization (the "initializing"
//     marker; writing it creates the data directory)
//  3. create the formal directories (Layout.Ensure)
//  4. open the database and apply all schema migrations
//  5. open the credential vault
//  6. write the default settings
//  7. verify: database integrity check, vault re-open, directory
//     structure
//  8. write the "complete" marker (which also clears the
//     in-progress state — both live in the same marker file)
//
// Recording the start FIRST closes the crash window between creating
// directories and writing the marker: even a crash inside step 2
// itself (data directory created, marker file not yet renamed into
// place, a torn temp file left behind) is recognized at the next
// launch, because installstate.Detect classifies any formal residue
// without a database as an interrupted initialization and startup
// cleans it back to exe-only.
//
// A new-user initialization starts from an installation with no user
// data, so on ANY failure the function closes what it opened and
// removes exactly the files and directories this run created,
// restoring the folder to exe-only before returning the error. The
// returned error names the failed step and states the cleanup
// outcome; if cleanup itself failed, the error says so explicitly
// instead of claiming the folder was restored. If the process dies
// mid-initialization, the "initializing" marker lets the next launch
// detect and clean up the interruption (see internal/installstate).
package initialize

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/settings"
)

// NewUser initializes a fresh installation under the given layout.
// The layout must be uninitialized (see installstate.Detect); NewUser
// does not check for or migrate any pre-existing or external data.
func NewUser(l paths.Layout, log *slog.Logger) error {
	before := takeSnapshot(l)

	fail := func(step string, err error) error {
		if cerr := before.cleanup(l); cerr != nil {
			log.Error("initialization failed and cleanup was incomplete",
				"step", step, "error", err, "cleanupError", cerr)
			return fmt.Errorf("初始化在「%s」失败：%w；自动清理未完全成功（%v），主程序目录可能仍有残留文件", step, err, cerr)
		}
		log.Warn("initialization failed; created files cleaned up", "step", step, "error", err)
		return fmt.Errorf("初始化在「%s」失败：%w；本次创建的文件已清理，可重新尝试", step, err)
	}

	// 1-2. Record the start of initialization FIRST (this creates
	// the data directory and atomically renames the marker into
	// place), then create the remaining formal directories.
	if err := installstate.MarkInitializing(l); err != nil {
		return fail("记录初始化开始", err)
	}
	if err := l.Ensure(); err != nil {
		return fail("创建数据目录", err)
	}
	log.Info("initialization started", "root", l.Root)

	// 3. Database: open and apply every schema migration.
	db, err := database.OpenWithOptions(l.DatabaseFile, database.Options{BackupDir: l.Backups})
	if err != nil {
		return fail("打开数据库", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.Migrate(ctx)
	cancel()
	if err != nil {
		_ = db.Close()
		return fail("执行数据库迁移", err)
	}

	// 4. Credential vault: it must open cleanly on the fresh layout.
	if _, _, err := secrets.Open(l.SecretsFile); err != nil {
		_ = db.Close()
		return fail("初始化凭据库", err)
	}

	// 5. Default settings.
	if err := settings.Save(l.SettingsFile, settings.Default()); err != nil {
		_ = db.Close()
		return fail("保存默认设置", err)
	}

	// 6-7. Verification: database integrity, vault re-open, layout.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.Health(ctx); err != nil {
		_ = db.Close()
		return fail("校验数据库", err)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		_ = db.Close()
		return fail("校验数据库完整性", err)
	}
	if integrity != "ok" {
		_ = db.Close()
		return fail("校验数据库完整性", fmt.Errorf("integrity_check reported %q", integrity))
	}
	if _, _, err := secrets.Open(l.SecretsFile); err != nil {
		_ = db.Close()
		return fail("校验凭据库", err)
	}
	for _, dir := range []string{l.Data, l.Config, l.Profiles, l.Backups, l.Logs, l.Mihomo} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			_ = db.Close()
			return fail("校验数据目录结构", fmt.Errorf("expected directory %s", dir))
		}
	}
	if info, err := os.Stat(l.DatabaseFile); err != nil || info.IsDir() {
		_ = db.Close()
		return fail("校验数据目录结构", fmt.Errorf("expected database file %s", l.DatabaseFile))
	}

	// 8. Completion marker; close the database last, once nothing can
	// fail any more.
	if err := installstate.MarkComplete(l); err != nil {
		_ = db.Close()
		return fail("写入完成标记", err)
	}
	if err := db.Close(); err != nil {
		// Initialization is complete and the marker says so; a close
		// error here must not trigger cleanup of a healthy install.
		log.Warn("close database after initialization failed", "error", err)
	}
	log.Info("initialization complete", "root", l.Root)
	return nil
}

// snapshot records which of the formal directories and which files
// and subdirectories already existed before initialization, so a
// failed run removes only what it created itself.
type snapshot struct {
	dirs  map[string]bool // existing directories (the roots and below)
	files map[string]bool // existing files
}

func formalRoots(l paths.Layout) []string {
	return []string{l.Data, l.Config, l.WebView2}
}

func takeSnapshot(l paths.Layout) snapshot {
	snap := snapshot{dirs: map[string]bool{}, files: map[string]bool{}}
	for _, root := range formalRoots(l) {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // missing root: nothing to record
			}
			if d.IsDir() {
				snap.dirs[path] = true
			} else {
				snap.files[path] = true
			}
			return nil
		})
	}
	return snap
}

// cleanup removes every file created since the snapshot and every
// directory created since the snapshot (deepest first), then
// verifies nothing created by the failed run remains. Pre-existing
// files and directories are never touched.
func (s snapshot) cleanup(l paths.Layout) error {
	var createdFiles, createdDirs []string
	for _, root := range formalRoots(l) {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if !s.dirs[path] {
					createdDirs = append(createdDirs, path)
				}
			} else if !s.files[path] {
				createdFiles = append(createdFiles, path)
			}
			return nil
		})
	}
	var errs []error
	for _, f := range createdFiles {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", f, err))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(createdDirs)))
	for _, d := range createdDirs {
		// os.Remove only removes empty directories; a directory that
		// still holds a pre-existing file (impossible for a directory
		// created by this run, but checked anyway) is left in place
		// and reported by the verification below.
		if err := os.Remove(d); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", d, err))
		}
	}
	// Verification: nothing the failed run created may remain.
	for _, root := range formalRoots(l) {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if !s.dirs[path] {
					errs = append(errs, fmt.Errorf("directory created by failed initialization still exists: %s", path))
				}
			} else if !s.files[path] {
				errs = append(errs, fmt.Errorf("file created by failed initialization still exists: %s", path))
			}
			return nil
		})
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
