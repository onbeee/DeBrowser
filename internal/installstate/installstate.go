// Package installstate owns the Stage 5A first-launch state: whether
// this portable installation has been initialized, and the temporary
// workspace the welcome flow runs in before any formal data exists.
//
// The state lives in a lightweight marker file (data/install.json,
// see paths.Layout.InstallFile) that records only the initialization
// state and the data-layout version. The data-layout version is
// managed separately from the application version: the layout changes
// only when the on-disk structure changes, not on every release.
//
// State rules (Stage 5A):
//   - marker "complete"                     -> initialized
//   - marker "initializing"                  -> a new-user initialization
//     was interrupted; its artifacts are not user data and are cleaned
//     up before the welcome page is shown again
//   - no marker, but the formal database file exists -> initialized
//     (installations created before the marker existed)
//   - no marker and no database              -> not initialized
//
// Nothing in this package scans %APPDATA%, %LOCALAPPDATA% or any
// other location: the state is derived exclusively from this portable
// installation's own directory.
package installstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/onbeee/DeBrowser/internal/paths"
)

// LayoutVersion is the current data-layout version recorded in the
// marker. Bump it only when the formal on-disk layout changes.
const LayoutVersion = 1

// Marker states stored in install.json.
const (
	StateInitializing = "initializing"
	StateComplete     = "complete"
)

// Phase is the startup classification of an installation.
type Phase int

const (
	// PhaseUninitialized: no marker and no formal database — the
	// first-launch welcome page is shown and nothing is created.
	PhaseUninitialized Phase = iota
	// PhaseInitializing: a previous new-user initialization did not
	// complete; its partial artifacts must be cleaned up first.
	PhaseInitializing
	// PhaseReady: the installation is initialized; start normally.
	PhaseReady
)

// Marker is the content of data/install.json.
type Marker struct {
	State         string `json:"state"`
	LayoutVersion int    `json:"layoutVersion"`
	UpdatedAt     string `json:"updatedAt"`
}

// ReadMarker reads the marker file. exists reports whether the file is
// present at all; a present but unreadable or malformed file returns
// an error with exists=true. A marker that cannot even be stated
// (missing, or a path component is not a directory) counts as absent.
func ReadMarker(l paths.Layout) (marker Marker, exists bool, err error) {
	if _, err := os.Stat(l.InstallFile); err != nil {
		return Marker{}, false, nil
	}
	raw, err := os.ReadFile(l.InstallFile)
	if err != nil {
		return Marker{}, true, fmt.Errorf("read install marker: %w", err)
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return Marker{}, true, fmt.Errorf("parse install marker: %w", err)
	}
	return marker, true, nil
}

// Detect classifies the installation for startup. It only stats and
// reads files inside the portable layout; it creates nothing.
func Detect(l paths.Layout) Phase {
	marker, exists, err := ReadMarker(l)
	if err == nil && exists {
		switch marker.State {
		case StateComplete:
			return PhaseReady
		case StateInitializing:
			// The marker is written before the database is created,
			// so it wins over the database check below: anything in
			// the formal directories was made by the interrupted
			// initialization and holds no user data.
			return PhaseInitializing
		}
		// An unknown state value is treated like an interrupted
		// initialization unless a database proves otherwise below.
	}
	if fileExists(l.DatabaseFile) {
		return PhaseReady
	}
	if exists {
		// A marker file is present (corrupt or unknown state) but no
		// database ever completed: an initialization was attempted
		// and interrupted.
		return PhaseInitializing
	}
	return PhaseUninitialized
}

// MarkInitializing records that a new-user initialization is in
// progress. MarkComplete records its successful completion. Both
// write atomically (temporary file plus rename inside data/), so a
// crash mid-write leaves either the old marker or none, never a torn
// file.
func MarkInitializing(l paths.Layout) error { return writeMarker(l, StateInitializing) }

// MarkComplete records that initialization completed successfully.
func MarkComplete(l paths.Layout) error { return writeMarker(l, StateComplete) }

func writeMarker(l paths.Layout, state string) error {
	if err := os.MkdirAll(l.Data, 0o750); err != nil {
		return fmt.Errorf("create data directory for install marker: %w", err)
	}
	marker := Marker{
		State:         state,
		LayoutVersion: LayoutVersion,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode install marker: %w", err)
	}
	tmp, err := os.CreateTemp(l.Data, ".install-*.tmp")
	if err != nil {
		return fmt.Errorf("create install marker: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write install marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write install marker: %w", err)
	}
	if err := os.Rename(tmpName, l.InstallFile); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace install marker: %w", err)
	}
	return nil
}

// CleanupFormal removes the formal data, config and WebView2
// directories completely and verifies they are gone. It is used to
// recover from an interrupted initialization (whose marker proves no
// completed initialization ever produced user data in them) and must
// never be called for an initialized installation. A failure is
// reported with the offending paths — the caller must surface it
// instead of pretending the directory is back to exe-only.
func CleanupFormal(l paths.Layout) error {
	var errs []error
	for _, dir := range []string{l.Data, l.Config, l.WebView2} {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
		}
	}
	for _, dir := range []string{l.Data, l.Config, l.WebView2} {
		if _, err := os.Stat(dir); err == nil {
			errs = append(errs, fmt.Errorf("directory still exists after cleanup: %s", dir))
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("verify cleanup of %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

// Workspace is the temporary runtime location of the welcome flow.
// The first-launch window needs WebView2 user data, a log file and
// single-instance coordination files before any formal directory may
// exist; all of them live under the operating system's temporary
// directory, in a per-installation folder derived from the portable
// root, and are removed when the welcome process exits.
type Workspace struct {
	Dir            string // <tmp>/DeBrowser/welcome-<hash>
	WebView2       string // <Dir>/webview2-data
	LockFile       string // <Dir>/app.lock
	ActivationFile string // <Dir>/activate.request
	LogFile        string // <Dir>/debrowser.log
}

// TempWorkspace resolves and creates the welcome workspace for the
// given application root. The name is deterministic per root, so a
// second launch of the same installation finds (and is rejected by)
// the first launch's single-instance lock, while separate portable
// installations never collide.
func TempWorkspace(root string) (Workspace, error) {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	dir := filepath.Join(os.TempDir(), "DeBrowser", "welcome-"+hex.EncodeToString(sum[:6]))
	ws := Workspace{
		Dir:            dir,
		WebView2:       filepath.Join(dir, "webview2-data"),
		LockFile:       filepath.Join(dir, "app.lock"),
		ActivationFile: filepath.Join(dir, "activate.request"),
		LogFile:        filepath.Join(dir, "debrowser.log"),
	}
	if err := os.MkdirAll(ws.WebView2, 0o750); err != nil {
		return Workspace{}, fmt.Errorf("create welcome workspace: %w", err)
	}
	return ws, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
