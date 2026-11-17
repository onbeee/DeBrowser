package chromebookmarks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A realistic Chromium Bookmarks document: standard roots plus a
// custom root, unknown fields at several levels, meta_info on a
// bookmark, and full-precision Chrome epoch timestamps.
const fixture = `{
  "checksum": "9f8e7d6c5b4a39281706f5e4d3c2b1a0",
  "roots": {
    "bookmark_bar": {
      "children": [
        {
          "date_added": "13350000000000001",
          "date_last_used": "0",
          "guid": "aaaaaaa1-1111-4111-8111-111111111111",
          "id": "5",
          "meta_info": { "custom_chrome_field": "keep-me" },
          "name": "Example",
          "type": "url",
          "url": "https://example.com/"
        },
        {
          "children": [
            {
              "date_added": "13350000000000002",
              "date_last_used": "0",
              "guid": "aaaaaaa2-2222-4222-8222-222222222222",
              "id": "7",
              "name": "Nested",
              "type": "url",
              "url": "https://nested.example/"
            }
          ],
          "date_added": "13349000000000000",
          "date_modified": "13350000000000002",
          "guid": "aaaaaaa3-3333-4333-8333-333333333333",
          "id": "6",
          "name": "Folder A",
          "type": "folder"
        }
      ],
      "date_added": "13300000000000000",
      "date_modified": "13350000000000002",
      "guid": "aaaaaaa0-0000-4000-8000-000000000000",
      "id": "1",
      "name": "Bookmarks bar",
      "type": "folder"
    },
    "other": {
      "children": [],
      "date_added": "13300000000000000",
      "date_modified": "13300000000000000",
      "guid": "bbbbbbb0-0000-4000-8000-000000000000",
      "id": "2",
      "name": "Other bookmarks",
      "type": "folder"
    },
    "synced": {
      "children": [],
      "date_added": "13300000000000000",
      "date_modified": "13300000000000000",
      "guid": "ccccccc0-0000-4000-8000-000000000000",
      "id": "3",
      "name": "Mobile bookmarks",
      "type": "folder"
    },
    "custom_root": {
      "children": [],
      "date_added": "13300000000000000",
      "guid": "ddddddd0-0000-4000-8000-000000000000",
      "id": "4",
      "name": "Custom root",
      "type": "folder",
      "x_unknown_root_field": true
    }
  },
  "version": 1,
  "x_top_unknown": { "keep": 1 }
}`

func mustParse(t *testing.T, data string) *File {
	t.Helper()
	f, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParsePreservesEverythingOnRoundTrip(t *testing.T) {
	f := mustParse(t, fixture)
	out, err := f.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"checksum": "9f8e7d6c5b4a39281706f5e4d3c2b1a0"`,
		`"custom_chrome_field": "keep-me"`,
		`"x_unknown_root_field": true`,
		`"x_top_unknown"`,
		`"date_added": "13350000000000001"`, // full precision kept
		`"guid": "aaaaaaa2-2222-4222-8222-222222222222"`,
		`"name": "Custom root"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("round trip lost %s", want)
		}
	}
	if f.CountBookmarks() != 2 {
		t.Errorf("CountBookmarks = %d, want 2", f.CountBookmarks())
	}
}

func TestWalkAndFind(t *testing.T) {
	f := mustParse(t, fixture)
	var folders []FlatFolder
	f.WalkFolders(func(fl FlatFolder) { folders = append(folders, fl) })
	if len(folders) != 1 || folders[0].Name != "Folder A" || folders[0].RootKey != "bookmark_bar" {
		t.Fatalf("folders = %+v", folders)
	}
	node, parent, rootKey, ok := f.FindByID("7")
	if !ok || rootKey != "bookmark_bar" || NameOf(node) != "Nested" || NameOf(parent) != "Folder A" {
		t.Fatalf("FindByID(7) = ok %v root %q", ok, rootKey)
	}
	var marks []FlatBookmark
	f.WalkBookmarks(func(b FlatBookmark) { marks = append(marks, b) })
	if len(marks) != 2 {
		t.Fatalf("marks = %d", len(marks))
	}
	for _, m := range marks {
		if m.Name == "Nested" && (len(m.Path) != 1 || m.Path[0] != "Folder A") {
			t.Errorf("Nested path = %v", m.Path)
		}
	}
}

func TestParseRejectsForeignDocuments(t *testing.T) {
	for _, bad := range []string{
		`{"hello": "world"}`,
		`{"roots": []}`,
		`{"roots": {"bookmark_bar": {"type": "url"}}}`,
		`not json at all`,
		`[1,2,3]`,
		// Trailing content after the document.
		fixture + `{}`,
		fixture + `garbage`,
		// A child that is not an object must not be silently dropped.
		`{"roots": {"bookmark_bar": {"type": "folder", "children": [42]}}}`,
		// Folder without a children array.
		`{"roots": {"bookmark_bar": {"type": "folder"}}}`,
		// Folder whose children is not an array.
		`{"roots": {"bookmark_bar": {"type": "folder", "children": {}}}}`,
		// Url node without a string url.
		`{"roots": {"bookmark_bar": {"type": "folder", "children": [{"type": "url", "name": "x"}]}}}`,
		// Unrecognized node type.
		`{"roots": {"bookmark_bar": {"type": "folder", "children": [{"type": "separator", "name": "x"}]}}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%.60s...) succeeded, want error", bad)
		}
	}
}

func TestLoadMissingFileFailsAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	// A profile without a Bookmarks file cannot be confirmed as
	// Chromium: Load must refuse, and must not invent a file.
	if _, err := Load(dir); !errors.Is(err, ErrNotChromium) {
		t.Fatalf("Load error = %v, want ErrNotChromium", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Error("a Bookmarks file was created for an unconfirmed profile")
	}
}

func TestSaveKeepsRollingBackupOfOriginal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	bar := file.Root("bookmark_bar")
	if ChildHasURL(bar, "https://example.com/") != true {
		t.Fatal("fixture bookmark not found at bar level")
	}
	AppendChild(bar, file.NewBookmark("Added", "https://added.example/"))
	if err := Save(dir, file); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(dir, backupName))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != fixture {
		t.Error("recovery copy does not hold the exact original bytes")
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.CountBookmarks() != 3 {
		t.Fatalf("count after add = %d, want 3", back.CountBookmarks())
	}
	// Node ids stayed stable and unique.
	seen := map[string]bool{}
	back.WalkBookmarks(func(b FlatBookmark) {
		if seen[b.NodeID] {
			t.Errorf("duplicate node id %q", b.NodeID)
		}
		seen[b.NodeID] = true
	})
	if !seen["5"] || !seen["7"] {
		t.Errorf("original ids lost: %v", seen)
	}
}

func TestCorruptFileLoadFailsAndIsLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	broken := []byte(`{"roots": {"bookmark_bar": {"type": "folder", "children": [`)
	if err := os.WriteFile(filepath.Join(dir, FileName), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load of corrupt file succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(after) != string(broken) {
		t.Error("corrupt file was modified")
	}
}

func writeFixtureProfile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSaveFailsWhenRecoveryCopyCannotBeWritten(t *testing.T) {
	dir := writeFixtureProfile(t)
	// A directory squatting on the recovery copy's name makes its
	// safe write (staging + rename) fail.
	if err := os.Mkdir(filepath.Join(dir, backupName), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	AppendChild(file.Root("bookmark_bar"), file.NewBookmark("X", "https://x.example/"))
	if err := Save(dir, file); err == nil {
		t.Fatal("Save succeeded despite an unwritable recovery copy")
	}
	after, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(after) != fixture {
		t.Error("original file changed after failed Save")
	}
}

func TestSaveFailsWhenReplacementCannotLand(t *testing.T) {
	dir := t.TempDir()
	// The target path is a directory: the atomic rename must fail
	// and the error must surface.
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	file := mustParse(t, fixture)
	if err := Save(dir, file); err == nil {
		t.Fatal("Save succeeded despite an unreplaceable target")
	}
	if info, err := os.Stat(filepath.Join(dir, FileName)); err != nil || !info.IsDir() {
		t.Error("target directory was disturbed")
	}
}

func TestSaveRestoresOriginalWhenFinalVerificationFails(t *testing.T) {
	dir := writeFixtureProfile(t)
	file, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	AppendChild(file.Root("bookmark_bar"), file.NewBookmark("X", "https://x.example/"))
	postReplaceHook = func(d string) {
		_ = os.WriteFile(filepath.Join(d, FileName), []byte("{corrupted"), 0o600)
	}
	defer func() { postReplaceHook = nil }()
	err = Save(dir, file)
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("Save error = %v, want a failure reporting the restore", err)
	}
	after, rerr := os.ReadFile(filepath.Join(dir, FileName))
	if rerr != nil || string(after) != fixture {
		t.Error("original file was not restored byte-for-byte")
	}
}

func TestSaveReportsRestoreFailure(t *testing.T) {
	dir := writeFixtureProfile(t)
	file, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	postReplaceHook = func(d string) {
		// Corrupt both the landed file and the recovery copy.
		_ = os.WriteFile(filepath.Join(d, FileName), []byte("{corrupted"), 0o600)
		_ = os.WriteFile(filepath.Join(d, backupName), []byte("{also corrupted"), 0o600)
	}
	defer func() { postReplaceHook = nil }()
	err = Save(dir, file)
	if err == nil || !strings.Contains(err.Error(), "restore failed") {
		t.Fatalf("Save error = %v, want a failure reporting the failed restore", err)
	}
}

func TestFolderReuseAndRemove(t *testing.T) {
	f := mustParse(t, fixture)
	bar := f.Root("bookmark_bar")
	if got := FindChildFolder(bar, "Folder A"); got == nil {
		t.Fatal("Folder A not found for reuse")
	}
	if got := FindChildFolder(bar, "Missing"); got != nil {
		t.Fatal("phantom folder found")
	}
	node, parent, _, ok := f.FindByID("5")
	if !ok {
		t.Fatal("node 5 missing")
	}
	if !RemoveFromParent(parent, node) {
		t.Fatal("RemoveFromParent failed")
	}
	if f.CountBookmarks() != 1 {
		t.Fatalf("count = %d after remove, want 1", f.CountBookmarks())
	}
	// Other roots untouched.
	if f.Root("synced") == nil || f.Root("custom_root") == nil {
		t.Error("roots were dropped")
	}
}
