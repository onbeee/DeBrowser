//go:build windows

package browserprobe

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Windows candidate sources (Stage 9A). Detection reads regular
// system information only: the App Paths registry keys, the
// uninstall registry keys and the browsers' well-known install
// directories. It never scans the disk, never walks user folders
// and never starts a detected executable.

// Sources returns the Windows candidate sources in priority order
// (App Paths first: it is the shell's own resolution table).
func Sources() []CandidateSource {
	return []CandidateSource{
		appPathsSource{},
		uninstallSource{},
		commonDirsSource{},
	}
}

// NewInspector returns the Windows inspector.
func NewInspector() Inspector { return windowsInspector{} }

type windowsInspector struct{}

func (windowsInspector) Exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// --- App Paths ----------------------------------------------------

type appPathsSource struct{}

func (appPathsSource) Candidates(context.Context) ([]Candidate, error) {
	var out []Candidate
	targets := []struct {
		exe  string
		kind Kind
	}{
		{"chrome.exe", KindChrome},
		{"brave.exe", KindBrave},
	}
	roots := []struct {
		key   registry.Key
		scope Scope
	}{
		{registry.LOCAL_MACHINE, ScopeSystem},
		{registry.CURRENT_USER, ScopeUser},
	}
	for _, root := range roots {
		for _, t := range targets {
			k, err := registry.OpenKey(root.key,
				`SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+t.exe,
				registry.QUERY_VALUE)
			if err != nil {
				continue // missing key is normal, not an error
			}
			v, _, err := k.GetStringValue("")
			_ = k.Close()
			if err == nil && v != "" {
				out = append(out, Candidate{Path: unquote(strings.TrimSpace(v)), Scope: root.scope, KindHint: t.kind})
			}
		}
	}
	return out, nil
}

// --- Uninstall keys -----------------------------------------------

type uninstallSource struct{}

func (uninstallSource) Candidates(context.Context) ([]Candidate, error) {
	var out []Candidate
	roots := []struct {
		key   registry.Key
		scope Scope
	}{
		{registry.LOCAL_MACHINE, ScopeSystem},
		{registry.CURRENT_USER, ScopeUser},
	}
	const base = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	for _, root := range roots {
		for _, sub := range []string{base, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`} {
			k, err := registry.OpenKey(root.key, sub, registry.ENUMERATE_SUB_KEYS)
			if err != nil {
				continue
			}
			names, err := k.ReadSubKeyNames(-1)
			_ = k.Close()
			if err != nil {
				continue
			}
			for _, name := range names {
				sk, err := registry.OpenKey(root.key, sub+`\`+name, registry.QUERY_VALUE)
				if err != nil {
					continue
				}
				display, _, _ := sk.GetStringValue("DisplayName")
				loc, _, _ := sk.GetStringValue("InstallLocation")
				_ = sk.Close()
				kind, exeName, ok := matchUninstallEntry(display)
				if !ok || loc == "" {
					continue
				}
				out = append(out, Candidate{
					Path:     filepath.Join(unquote(strings.TrimSpace(loc)), exeName),
					Scope:    root.scope,
					KindHint: kind,
				})
			}
		}
	}
	return out, nil
}

// matchUninstallEntry maps an uninstall DisplayName to a supported
// browser. Only Chrome Stable and Brave Stable are in scope.
func matchUninstallEntry(display string) (Kind, string, bool) {
	d := strings.ToLower(display)
	switch {
	case strings.Contains(d, "google chrome"):
		return KindChrome, "chrome.exe", true
	case strings.Contains(d, "brave"):
		return KindBrave, "brave.exe", true
	}
	return "", "", false
}

// --- Well-known install directories -------------------------------

type commonDirsSource struct{}

func (commonDirsSource) Candidates(context.Context) ([]Candidate, error) {
	var out []Candidate
	type dir struct {
		base  string
		scope Scope
	}
	var bases []dir
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if v := os.Getenv(env); v != "" {
			bases = append(bases, dir{v, ScopeSystem})
		}
	}
	if v := os.Getenv("LocalAppData"); v != "" {
		bases = append(bases, dir{v, ScopeUser})
	}
	rel := []struct {
		sub  string
		kind Kind
	}{
		{`Google\Chrome\Application\chrome.exe`, KindChrome},
		{`BraveSoftware\Brave-Browser\Application\brave.exe`, KindBrave},
	}
	for _, b := range bases {
		for _, r := range rel {
			out = append(out, Candidate{Path: filepath.Join(b.base, r.sub), Scope: b.scope, KindHint: r.kind})
		}
	}
	return out, nil
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
