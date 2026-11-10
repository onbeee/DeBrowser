package config

import (
	"os"
	"path/filepath"
	"testing"
)

func envWithRoot(root string) func(string) string {
	return func(key string) string {
		if key == EnvAppRoot {
			return root
		}
		return ""
	}
}

func TestLoadPortableLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "DeBrowser-windows-amd64")
	cfg, err := Load(envWithRoot(root))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	l := cfg.Layout
	if l.Root != root {
		t.Errorf("root = %s, want %s", l.Root, root)
	}
	if l.DatabaseFile != filepath.Join(root, "data", "debrowser.db") {
		t.Errorf("database = %s", l.DatabaseFile)
	}
	if l.WebView2 != filepath.Join(root, "webview2-data") {
		t.Errorf("webview2 = %s", l.WebView2)
	}
	if l.SettingsFile != filepath.Join(root, "config", "settings.json") {
		t.Errorf("settings = %s", l.SettingsFile)
	}
	if _, err := os.Stat(l.Data); err != nil {
		t.Errorf("data directory not created: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log level = %s", cfg.LogLevel)
	}
}

func TestLoadRejectsBadLogLevel(t *testing.T) {
	env := func(key string) string {
		switch key {
		case EnvAppRoot:
			return t.TempDir()
		case "DEBROWSER_LOG_LEVEL":
			return "chatty"
		}
		return ""
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted an invalid log level")
	}
}

func TestLoadFailsOnUnwritableRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(envWithRoot(root)); err == nil {
		t.Fatal("Load succeeded despite an unusable application root")
	}
}
