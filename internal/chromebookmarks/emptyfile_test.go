package chromebookmarks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing Bookmarks file inside an established profile is the
// legitimate empty state: LoadOrEmpty answers an empty document
// with the standard Chromium structure, while strict Load keeps
// refusing to invent one.
func TestLoadOrEmptyMissingFileReturnsEmptyDocument(t *testing.T) {
	dir := t.TempDir()
	f, err := LoadOrEmpty(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.CountBookmarks() != 0 {
		t.Fatalf("count = %d, want 0", f.CountBookmarks())
	}
	for _, key := range []string{"bookmark_bar", "other", "synced"} {
		root := f.Root(key)
		if root == nil {
			t.Fatalf("empty document lacks root %q", key)
		}
		if nodeType(root) != "folder" || len(Children(root)) != 0 {
			t.Fatalf("root %q is not an empty folder", key)
		}
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("strict Load must still fail for a missing file")
	}
	// The first node created in an empty document continues the
	// root id sequence (roots hold ids 1-3).
	nb := f.NewBookmark("X", "https://x.example/")
	if id := nodeID(nb); id != "4" {
		t.Fatalf("first new node id = %q, want 4", id)
	}
}

// The empty document serializes into a document Parse accepts and
// Chromium would recognize: roots object, version 1, folder nodes
// carrying id/guid/timestamps/children.
func TestEmptyFileSerializesToValidChromiumStructure(t *testing.T) {
	data, err := EmptyFile().Serialize()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc["version"].(float64); !ok || v != 1 {
		t.Fatalf("version = %v, want 1", doc["version"])
	}
	roots, ok := doc["roots"].(map[string]any)
	if !ok || len(roots) != 3 {
		t.Fatalf("roots = %v, want exactly the three standard roots", doc["roots"])
	}
	for key, raw := range roots {
		node, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("root %q is not an object", key)
		}
		for _, field := range []string{"children", "date_added", "date_modified", "guid", "id", "name", "type"} {
			if _, ok := node[field]; !ok {
				t.Fatalf("root %q lacks field %q", key, field)
			}
		}
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("serialized empty document does not parse: %v", err)
	}
}

func TestLoadOrEmptyExistingFileLoads(t *testing.T) {
	dir := writeFixtureProfile(t)
	f, err := LoadOrEmpty(dir)
	if err != nil {
		t.Fatal(err)
	}
	strict, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.CountBookmarks() != strict.CountBookmarks() || f.CountBookmarks() == 0 {
		t.Fatalf("LoadOrEmpty count = %d, strict Load count = %d; want equal and non-zero", f.CountBookmarks(), strict.CountBookmarks())
	}
}

// A present but corrupt file is never disguised as an empty one.
func TestLoadOrEmptyCorruptFileFails(t *testing.T) {
	for _, content := range []string{"{broken json", `{"roots": 42}`, `{"version": 1}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrEmpty(dir); err == nil {
			t.Fatalf("content %q must fail, not read as empty", content)
		}
	}
}

// Save into a profile with no Bookmarks file creates it, with no
// recovery copy (there was no original to preserve) and no
// staging leftovers; the created file loads back through the
// strict path.
func TestSaveCreatesBookmarksFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	f, err := LoadOrEmpty(dir)
	if err != nil {
		t.Fatal(err)
	}
	AppendChild(f.EnsureBookmarkBar(), f.NewBookmark("New", "https://new.example/"))
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	after, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.CountBookmarks() != 1 {
		t.Fatalf("created file holds %d bookmarks, want 1", after.CountBookmarks())
	}
	if _, err := os.Stat(filepath.Join(dir, backupName)); !os.IsNotExist(err) {
		t.Fatal("no recovery copy may be made when there was no original file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "Bookmarks.tmp-") || strings.HasPrefix(e.Name(), "Bookmarks.stage-") {
			t.Fatalf("staging leftover %q", e.Name())
		}
	}
}

// When creating a file fails verification, the unverified new
// file is removed again: the profile returns to having no
// Bookmarks file rather than keeping a corrupt one.
func TestSaveNewFileVerifyFailureRemovesCreatedFile(t *testing.T) {
	dir := t.TempDir()
	f := EmptyFile()
	AppendChild(f.EnsureBookmarkBar(), f.NewBookmark("New", "https://new.example/"))
	postReplaceHook = func(d string) {
		_ = os.WriteFile(filepath.Join(d, FileName), []byte("{corrupted"), 0o600)
	}
	defer func() { postReplaceHook = nil }()
	err := Save(dir, f)
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("Save error = %v, want a failure reporting the removal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("the unverified new file was left behind")
	}
}
