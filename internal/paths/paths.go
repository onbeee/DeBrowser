// Package paths is the single source of truth for every filesystem
// location DeBrowser uses. DeBrowser is portable-first: all persistent
// data lives next to the executable, under the application root:
//
//	<root>\debrowser.exe
//	<root>\data\debrowser.db
//	<root>\data\backups\
//	<root>\data\logs\
//	<root>\data\profiles\
//	<root>\data\mihomo\
//	<root>\webview2-data\
//	<root>\config\settings.json
//
// Business modules must take their directories from Layout and never
// join paths themselves, so the whole application folder can be moved
// or copied to another drive or machine and keep working.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout describes every location under one application root.
type Layout struct {
	Root         string // application root (directory containing the executable)
	Data         string // <root>/data
	DatabaseFile string // <root>/data/debrowser.db
	Backups      string // <root>/data/backups
	Logs         string // <root>/data/logs
	LogFile      string // <root>/data/logs/debrowser.log
	Profiles     string // <root>/data/profiles
	Mihomo       string // <root>/data/mihomo
	WebView2     string // <root>/webview2-data
	Config       string // <root>/config
	SettingsFile string // <root>/config/settings.json
	// InstanceLock is the single-instance lock file for THIS portable
	// installation. Because it lives inside the portable directory,
	// separate installations in different folders never see each
	// other's lock and run independently.
	InstanceLock string // <root>/data/app.lock
	// ActivationFile is how a second launch asks the running instance
	// to focus its window (see internal/singleinstance).
	ActivationFile string // <root>/data/activate.request
}

// Resolve builds the portable layout for an application root directory.
// It is a pure function: it never consults the process working
// directory, so tests can pass any root (for example a D: or E: folder
// on Windows) and get a layout that stays entirely under that root.
func Resolve(root string) (Layout, error) {
	if root == "" {
		return Layout{}, fmt.Errorf("application root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve application root %q: %w", root, err)
	}
	data := filepath.Join(abs, "data")
	cfg := filepath.Join(abs, "config")
	return Layout{
		Root:           abs,
		Data:           data,
		DatabaseFile:   filepath.Join(data, "debrowser.db"),
		Backups:        filepath.Join(data, "backups"),
		Logs:           filepath.Join(data, "logs"),
		LogFile:        filepath.Join(data, "logs", "debrowser.log"),
		Profiles:       filepath.Join(data, "profiles"),
		Mihomo:         filepath.Join(data, "mihomo"),
		WebView2:       filepath.Join(abs, "webview2-data"),
		Config:         cfg,
		SettingsFile:   filepath.Join(cfg, "settings.json"),
		InstanceLock:   filepath.Join(data, "app.lock"),
		ActivationFile: filepath.Join(data, "activate.request"),
	}, nil
}

// ExecutableDir returns the directory containing the running
// executable, with symbolic links resolved. The current working
// directory is intentionally never used: DeBrowser is portable and its
// data must follow the executable wherever it is launched from.
func ExecutableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// Ensure creates every directory in the layout and verifies the
// application root is writable by creating and removing a probe file
// in each key directory. A read-only location is a hard startup error:
// DeBrowser never silently falls back to %APPDATA% or %LOCALAPPDATA%.
func (l Layout) Ensure() error {
	dirs := []string{l.Root, l.Data, l.Backups, l.Logs, l.Profiles, l.Mihomo, l.WebView2, l.Config}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
		if err := probeWritable(dir); err != nil {
			return err
		}
	}
	return nil
}

func probeWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".debrowser-write-probe-*")
	if err != nil {
		return fmt.Errorf("application directory is not writable: %s: %w", dir, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("application directory is not writable: %s: %w", dir, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("application directory is not writable: %s: %w", dir, err)
	}
	return nil
}
