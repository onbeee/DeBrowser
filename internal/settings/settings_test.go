package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsChineseDark(t *testing.T) {
	s := Default()
	if s.Language != "zh-CN" {
		t.Errorf("default language = %q, want zh-CN", s.Language)
	}
	if s.Theme != ThemeDark {
		t.Errorf("default theme = %q, want dark", s.Theme)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if s != Default() {
		t.Errorf("missing file settings = %+v, want defaults %+v", s, Default())
	}
}

func TestSaveLoadRoundtripPersistsTheme(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	for _, theme := range []string{ThemeLight, ThemeWarm, ThemeDark} {
		want := Settings{Language: LanguageZhCN, Theme: theme}
		if err := Save(file, want); err != nil {
			t.Fatalf("Save(%s): %v", theme, err)
		}
		got, err := Load(file)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != want {
			t.Errorf("roundtrip = %+v, want %+v", got, want)
		}
	}
}

func TestSaveRejectsInvalidValues(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := Save(file, Settings{Language: "en-US", Theme: ThemeDark}); err == nil {
		t.Error("Save accepted an unsupported language")
	}
	if err := Save(file, Settings{Language: LanguageZhCN, Theme: "blue"}); err == nil {
		t.Error("Save accepted an unsupported theme")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("invalid settings were written to disk")
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil {
		t.Error("Load accepted a corrupt settings file")
	}
}

func TestLogCapacityRoundTripAndLegacyFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	// A legacy settings file without the capacity field loads as
	// "not configured" (0) instead of failing.
	if err := os.WriteFile(file, []byte("{\"language\":\"zh-CN\",\"theme\":\"dark\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatalf("legacy settings rejected: %v", err)
	}
	if loaded.LogCapacityBytes != 0 {
		t.Fatalf("legacy capacity = %d, want 0 (unset)", loaded.LogCapacityBytes)
	}
	// A configured capacity round-trips.
	s := Default()
	s.LogCapacityBytes = 50 << 20
	if err := Save(file, s); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LogCapacityBytes != 50<<20 {
		t.Fatalf("capacity round-trip = %d", loaded.LogCapacityBytes)
	}
	// Out-of-range capacities are invalid.
	s.LogCapacityBytes = 100
	if err := s.Validate(); err == nil {
		t.Fatal("tiny capacity validated")
	}
	s.LogCapacityBytes = 2 << 40
	if err := s.Validate(); err == nil {
		t.Fatal("huge capacity validated")
	}
}
