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
