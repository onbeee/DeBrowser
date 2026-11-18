package chromebookmarks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalStateName is the Chromium user-data-level state file. It
// records, among other things, which profile directory was used
// most recently ("profile.last_used").
const LocalStateName = "Local State"

// defaultProfileName is the only fallback profile directory name:
// Chromium's primary profile is always called "Default".
const defaultProfileName = "Default"

// ResolveProfileDir maps a Chromium user data directory (the
// --user-data-dir an instance was launched with) to the actual
// profile directory that holds its Bookmarks file.
//
// The Bookmarks file does NOT live in the user data directory
// itself; it lives in a profile subdirectory ("Default",
// "Profile 1", ...). The authoritative source for which one is
// the instance's profile is Local State's profile.last_used.
//
// Rules, deliberately strict — a wrong guess would read or write
// another profile's bookmarks:
//
//   - Local State present and valid, with a non-empty
//     profile.last_used: that name must be a single safe relative
//     directory name and the directory must exist. Any failure is
//     an error — there is NO fallback and NO scanning of other
//     profiles, because Chromium has told us exactly which profile
//     is in use and anything else would be a guess.
//   - Local State missing, or present without a usable last_used
//     record: fall back to "Default", and only when the Default
//     directory exists. No other profile directory is ever
//     considered, so the fallback is unambiguous.
//   - Local State present but not valid JSON: an error. A corrupt
//     state file is never silently second-guessed.
//
// Whether the profile already holds a Bookmarks file is NOT part
// of validity: Chromium creates that file only once the user has
// bookmarks, so a freshly used profile legitimately has none. A
// missing Bookmarks file inside a resolved profile means "no
// bookmarks yet" and is handled by LoadOrEmpty/Save; an existing
// but corrupt file still fails there. ResolveProfileDir never
// creates directories or files.
func ResolveProfileDir(userDataDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(userDataDir, LocalStateName))
	switch {
	case err == nil:
		lastUsed, perr := parseLastUsed(data)
		if perr != nil {
			return "", perr
		}
		if lastUsed != "" {
			return checkedProfileDir(userDataDir, lastUsed)
		}
		// No usable last_used record: unambiguous Default
		// fallback below.
	case os.IsNotExist(err):
		// No Local State at all: unambiguous Default fallback.
	default:
		return "", fmt.Errorf("chromebookmarks: read %s: %w", LocalStateName, err)
	}
	dir, err := checkedProfileDir(userDataDir, defaultProfileName)
	if err != nil {
		return "", fmt.Errorf("chromebookmarks: no usable %s record and the default profile is not usable: %w", LocalStateName, err)
	}
	return dir, nil
}

// parseLastUsed extracts profile.last_used from a Local State
// document. The document must be exactly one JSON value; a
// malformed document is an error, never a fallback trigger.
func parseLastUsed(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var doc struct {
		Profile struct {
			LastUsed string `json:"last_used"`
		} `json:"profile"`
	}
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("chromebookmarks: %s is not valid JSON: %w", LocalStateName, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("chromebookmarks: %s has trailing content after the JSON document", LocalStateName)
	}
	return strings.TrimSpace(doc.Profile.LastUsed), nil
}

// checkedProfileDir validates one candidate profile directory
// name under userDataDir and returns its path. The name must be a
// single relative path element (no separators, no "." or "..",
// never absolute) and the directory must exist. The Bookmarks
// file inside it is deliberately NOT checked: its absence is a
// legitimate empty-bookmarks state, not an invalid profile.
func checkedProfileDir(userDataDir, name string) (string, error) {
	if name == "" || name == "." || name == ".." ||
		filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) ||
		filepath.Base(name) != name {
		return "", fmt.Errorf("chromebookmarks: unsafe profile directory name %q in %s", name, LocalStateName)
	}
	dir := filepath.Join(userDataDir, name)
	// Defense in depth: the joined path must stay directly inside
	// the user data directory.
	if filepath.Dir(dir) != filepath.Clean(userDataDir) {
		return "", fmt.Errorf("chromebookmarks: profile directory %q escapes the user data directory", name)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("chromebookmarks: profile directory %q: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("chromebookmarks: profile directory %q is not a directory", name)
	}
	return dir, nil
}
