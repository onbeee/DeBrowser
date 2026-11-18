package chromebookmarks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localStateWithLastUsed(t *testing.T, name string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"profile": map[string]any{"last_used": name}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const profileFixture = `{"version":1,"roots":{"bookmark_bar":{"type":"folder","name":"Bookmarks bar","id":"1","children":[{"type":"url","name":"Example","url":"https://example.com/","id":"5"}]},"other":{"type":"folder","name":"Other","id":"2","children":[]},"synced":{"type":"folder","name":"Mobile","id":"3","children":[]}}}`

func writeProfile(t *testing.T, userDataDir, profile, content string) string {
	t.Helper()
	dir := filepath.Join(userDataDir, profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeLocalState(t *testing.T, userDataDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(userDataDir, LocalStateName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDefaultWithoutLocalState(t *testing.T) {
	udd := t.TempDir()
	want := writeProfile(t, udd, "Default", profileFixture)
	got, err := ResolveProfileDir(udd)
	if err != nil || got != want {
		t.Fatalf("resolve = %q, %v; want %q", got, err, want)
	}
	if f, err := Load(got); err != nil || f.CountBookmarks() != 1 {
		t.Fatalf("load resolved profile: %v", err)
	}
}

func TestResolveLastUsedDefault(t *testing.T) {
	udd := t.TempDir()
	want := writeProfile(t, udd, "Default", profileFixture)
	writeLocalState(t, udd, `{"profile":{"last_used":"Default"}}`)
	got, err := ResolveProfileDir(udd)
	if err != nil || got != want {
		t.Fatalf("resolve = %q, %v; want %q", got, err, want)
	}
}

func TestResolveLastUsedProfile1(t *testing.T) {
	udd := t.TempDir()
	writeProfile(t, udd, "Default", profileFixture)
	want := writeProfile(t, udd, "Profile 1", profileFixture)
	writeLocalState(t, udd, `{"profile":{"last_used":"Profile 1"}}`)
	got, err := ResolveProfileDir(udd)
	if err != nil || got != want {
		t.Fatalf("resolve = %q, %v; want Profile 1 dir %q", got, err, want)
	}
}

func TestResolveMultipleProfilesUsesOnlyLastUsed(t *testing.T) {
	udd := t.TempDir()
	writeProfile(t, udd, "Default", profileFixture)
	writeProfile(t, udd, "Profile 1", profileFixture)
	want := writeProfile(t, udd, "Profile 2", profileFixture)
	writeLocalState(t, udd, `{"profile":{"last_used":"Profile 2","info_cache":{}}}`)
	got, err := ResolveProfileDir(udd)
	if err != nil || got != want {
		t.Fatalf("resolve = %q, %v; want %q", got, err, want)
	}
}

func TestResolveFallbackWhenNoLastUsedRecord(t *testing.T) {
	udd := t.TempDir()
	want := writeProfile(t, udd, "Default", profileFixture)
	// Local State exists but carries no profile record at all.
	writeLocalState(t, udd, `{"browser":{"window_placement":{}}}`)
	got, err := ResolveProfileDir(udd)
	if err != nil || got != want {
		t.Fatalf("resolve = %q, %v; want Default fallback %q", got, err, want)
	}
}

func TestResolveCorruptLocalStateFails(t *testing.T) {
	udd := t.TempDir()
	writeProfile(t, udd, "Default", profileFixture)
	writeLocalState(t, udd, `{"profile": broken`)
	if _, err := ResolveProfileDir(udd); err == nil {
		t.Fatal("corrupt Local State must fail, never silently fall back")
	}
}

func TestResolveLastUsedMissingDirFails(t *testing.T) {
	udd := t.TempDir()
	writeProfile(t, udd, "Default", profileFixture)
	writeLocalState(t, udd, `{"profile":{"last_used":"Profile 9"}}`)
	if _, err := ResolveProfileDir(udd); err == nil {
		t.Fatal("last_used pointing at a missing directory must fail (no fallback, no scanning)")
	}
}

func TestResolveLastUsedTraversalRejected(t *testing.T) {
	for _, name := range []string{"..", "../Default", "Profile 1/../../etc", "/etc", `C:\Windows`, "a/b", `a\b`, "."} {
		udd := t.TempDir()
		writeProfile(t, udd, "Default", profileFixture)
		writeLocalState(t, udd, localStateWithLastUsed(t, name))
		if _, err := ResolveProfileDir(udd); err == nil {
			t.Fatalf("last_used %q must be rejected", name)
		}
	}
}

func TestResolveProfileMissingBookmarksFails(t *testing.T) {
	udd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(udd, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLocalState(t, udd, `{"profile":{"last_used":"Default"}}`)
	if _, err := ResolveProfileDir(udd); err == nil {
		t.Fatal("a profile without a Bookmarks file must fail")
	}
	// And nothing may have been created for it.
	if _, err := os.Stat(filepath.Join(udd, "Default", FileName)); !os.IsNotExist(err) {
		t.Fatal("resolution must never create a Bookmarks file")
	}
}

func TestResolveNoUsableProfileFails(t *testing.T) {
	udd := t.TempDir() // completely empty user data directory
	if _, err := ResolveProfileDir(udd); err == nil {
		t.Fatal("empty user data directory must fail")
	}
}

// Writes through a resolved profile land in that profile only:
// the backup sits next to the actual Bookmarks file and other
// profiles are untouched.
func TestSaveThroughResolvedProfile(t *testing.T) {
	udd := t.TempDir()
	defaultDir := writeProfile(t, udd, "Default", profileFixture)
	p1Dir := writeProfile(t, udd, "Profile 1", profileFixture)
	writeLocalState(t, udd, `{"profile":{"last_used":"Profile 1"}}`)

	resolved, err := ResolveProfileDir(udd)
	if err != nil || resolved != p1Dir {
		t.Fatalf("resolve = %q, %v", resolved, err)
	}
	unlock := LockProfile(resolved)
	defer unlock()
	f, err := Load(resolved)
	if err != nil {
		t.Fatal(err)
	}
	bar := f.EnsureBookmarkBar()
	AppendChild(bar, f.NewBookmark("Added", "https://added.example/"))
	if err := Save(resolved, f); err != nil {
		t.Fatal(err)
	}
	// Backup lives next to the actual Bookmarks file.
	if _, err := os.Stat(filepath.Join(p1Dir, backupName)); err != nil {
		t.Fatalf("recovery copy missing in the actual profile dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultDir, backupName)); !os.IsNotExist(err) {
		t.Fatal("recovery copy leaked into another profile")
	}
	// No stray staging files remain in the actual profile dir.
	entries, err := os.ReadDir(p1Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "Bookmarks.tmp-") || strings.HasPrefix(e.Name(), "Bookmarks.stage-") {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
	// The other profile's file is byte-identical to before.
	data, err := os.ReadFile(filepath.Join(defaultDir, FileName))
	if err != nil || string(data) != profileFixture {
		t.Fatal("another profile's Bookmarks file was modified")
	}
	// The resolved profile really gained the bookmark.
	after, err := Load(resolved)
	if err != nil || after.CountBookmarks() != 2 {
		t.Fatalf("resolved profile count after save: %v", err)
	}
}

func TestCorruptBookmarksInResolvedProfileFails(t *testing.T) {
	udd := t.TempDir()
	dir := writeProfile(t, udd, "Default", "{not json")
	resolved, err := ResolveProfileDir(udd)
	if err != nil || resolved != dir {
		t.Fatalf("resolve = %q, %v (existence is resolvable; content is Load's job)", resolved, err)
	}
	if _, err := Load(resolved); err == nil {
		t.Fatal("corrupt Bookmarks must fail to load")
	}
	// The corrupt file is left exactly as found.
	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(data) != "{not json" {
		t.Fatal("corrupt Bookmarks file was created over or modified")
	}
}
