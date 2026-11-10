// Package settings persists the small set of user preferences that
// live outside the database: UI language and appearance theme. The
// settings file is <root>/config/settings.json so it travels with the
// portable application folder.
//
// The UI language is Simplified Chinese only (zh-CN): there is no
// English language pack and no language switcher by product decision.
// The language field is still stored explicitly so the Settings page
// can display it and the file format stays self-describing.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LanguageZhCN is the only supported UI language.
const LanguageZhCN = "zh-CN"

// Supported appearance themes.
const (
	ThemeDark  = "dark"
	ThemeLight = "light"
	ThemeWarm  = "warm"
)

// Settings is the persisted user preference set.
type Settings struct {
	Language string `json:"language"`
	Theme    string `json:"theme"`
}

// Default returns the first-run settings: Simplified Chinese, dark.
func Default() Settings {
	return Settings{Language: LanguageZhCN, Theme: ThemeDark}
}

// Validate reports whether the settings hold supported values.
func (s Settings) Validate() error {
	if s.Language != LanguageZhCN {
		return fmt.Errorf("unsupported language %q (only %s is supported)", s.Language, LanguageZhCN)
	}
	switch s.Theme {
	case ThemeDark, ThemeLight, ThemeWarm:
		return nil
	default:
		return fmt.Errorf("unsupported theme %q", s.Theme)
	}
}

// Load reads settings from file. A missing file yields the defaults
// (first run); a present but invalid file is an explicit error so a
// corrupt settings file never silently resets user preferences.
func Load(file string) (Settings, error) {
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings %s: %w", file, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", file, err)
	}
	if err := s.Validate(); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", file, err)
	}
	return s, nil
}

// Save validates and writes settings atomically (write to a temporary
// file in the same directory, then rename over the target).
func Save(file string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("write settings %s: %w", file, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write settings %s: %w", file, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write settings %s: %w", file, err)
	}
	if err := os.Rename(tmpName, file); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace settings %s: %w", file, err)
	}
	return nil
}
