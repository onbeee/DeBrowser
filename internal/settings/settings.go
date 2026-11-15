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
	// LogCapacityBytes is the user's total log capacity limit
	// (unified recording system). Zero means "not configured":
	// the logging default (100 MB) applies. The field is absent
	// from settings files written before it existed, which decode
	// as zero — fully backward compatible.
	LogCapacityBytes int64 `json:"logCapacityBytes,omitempty"`
}

// Default returns the first-run settings: Simplified Chinese, dark.
func Default() Settings {
	return Settings{Language: LanguageZhCN, Theme: ThemeLight}
}

// Log capacity bounds mirrored from internal/logging (kept as
// literals here so the settings package stays dependency-free;
// internal/logging enforces the same bounds again).
const (
	minLogCapacityBytes = 1 << 20
	maxLogCapacityBytes = 1024 << 30
)

// Validate reports whether the settings hold supported values.
func (s Settings) Validate() error {
	if s.Language != LanguageZhCN {
		return fmt.Errorf("unsupported language %q (only %s is supported)", s.Language, LanguageZhCN)
	}
	switch s.Theme {
	case ThemeDark, ThemeLight, ThemeWarm:
	default:
		return fmt.Errorf("unsupported theme %q", s.Theme)
	}
	if s.LogCapacityBytes != 0 &&
		(s.LogCapacityBytes < minLogCapacityBytes || s.LogCapacityBytes > maxLogCapacityBytes) {
		return fmt.Errorf("unsupported log capacity %d", s.LogCapacityBytes)
	}
	return nil
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
