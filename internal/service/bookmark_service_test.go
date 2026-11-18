package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/chromebookmarks"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// bookmarkFixture is a small but real-structured Chromium
// bookmarks document used across the service tests.
const bookmarkFixture = `{
  "checksum": "abc",
  "roots": {
    "bookmark_bar": {
      "children": [
        {"date_added": "13350000000000001", "date_last_used": "0", "guid": "g-5", "id": "5", "name": "Shared", "type": "url", "url": "https://shared.example/"}
      ],
      "date_added": "13300000000000000", "date_modified": "13350000000000001", "guid": "g-1", "id": "1", "name": "Bookmarks bar", "type": "folder"
    },
    "other": {"children": [], "date_added": "13300000000000000", "date_modified": "13300000000000000", "guid": "g-2", "id": "2", "name": "Other bookmarks", "type": "folder"},
    "synced": {"children": [], "date_added": "13300000000000000", "date_modified": "13300000000000000", "guid": "g-3", "id": "3", "name": "Mobile bookmarks", "type": "folder"}
  },
  "version": 1
}`

func bookmarkTestServices(t *testing.T) (*BookmarkService, *InstanceService, *database.DB) {
	t.Helper()
	instSvc, _, db := testRuntimeService(t, newFakeLauncher())
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	return NewBookmarkService(db, instSvc, nil), instSvc, db
}

func makeInstance(t *testing.T, svc *InstanceService, name string) model.Instance {
	t.Helper()
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: name, BrowserID: "reg-a"})
	if err != nil {
		t.Fatalf("create instance %s: %v", name, err)
	}
	return inst
}

// profileDirOf returns the instance's actual Chromium profile
// directory for fixture purposes: the Default profile inside its
// user data directory. Fixtures carry no Local State, so service
// reads exercise the unambiguous Default fallback of
// chromebookmarks.ResolveProfileDir.
func profileDirOf(t *testing.T, svc *BookmarkService, inst model.Instance) string {
	t.Helper()
	_, dir, gate, err := svc.instances.InstanceBookmarkAccess(inst.ID)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if gate != BookmarkGateStopped {
		t.Fatalf("gate = %q, want stopped", gate)
	}
	return filepath.Join(dir, "Default")
}

func writeProfileBookmarks(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Bookmarks"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// freshProfileDir makes an instance look like a browser that has
// been launched (its Default profile directory and a Local State
// naming it last used both exist) but never given a bookmark: the
// profile holds no Bookmarks file. It returns the profile dir.
func freshProfileDir(t *testing.T, svc *BookmarkService, inst model.Instance) string {
	t.Helper()
	dir := profileDirOf(t, svc, inst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	udd := filepath.Dir(dir)
	if err := os.WriteFile(filepath.Join(udd, "Local State"), []byte(`{"profile":{"last_used":"Default"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// bookmarkFixtureTwo is a second valid document whose bookmark
// bar holds two bookmarks (A and B), for multi-profile fixtures.
const bookmarkFixtureTwo = `{
  "roots": {
    "bookmark_bar": {
      "children": [
        {"date_added": "13350000000000001", "date_last_used": "0", "guid": "g-5", "id": "5", "name": "A", "type": "url", "url": "https://a.example/"},
        {"date_added": "13350000000000002", "date_last_used": "0", "guid": "g-6", "id": "6", "name": "B", "type": "url", "url": "https://b.example/"}
      ],
      "date_added": "13300000000000000", "date_modified": "13350000000000001", "guid": "g-1", "id": "1", "name": "Bookmarks bar", "type": "folder"
    },
    "other": {"children": [], "date_added": "13300000000000000", "date_modified": "13300000000000000", "guid": "g-2", "id": "2", "name": "Other bookmarks", "type": "folder"},
    "synced": {"children": [], "date_added": "13300000000000000", "date_modified": "13300000000000000", "guid": "g-3", "id": "3", "name": "Mobile bookmarks", "type": "folder"}
  },
  "version": 1
}`

func readProfileBookmarks(t *testing.T, dir string) *chromebookmarks.File {
	t.Helper()
	file, err := chromebookmarks.Load(dir)
	if err != nil {
		t.Fatalf("load profile bookmarks: %v", err)
	}
	return file
}

func TestLibraryCRUDAndValidation(t *testing.T) {
	bs, _, _ := bookmarkTestServices(t)

	if _, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: " ", URL: "https://a.example/"}); err == nil {
		t.Error("empty name accepted")
	}
	if _, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "x", URL: "not a url"}); err == nil {
		t.Error("invalid URL accepted")
	}
	if _, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "x", URL: "ftp://a.example/"}); err == nil {
		t.Error("non-http URL accepted")
	}
	folder, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "folder", Name: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "folder", Name: "Sub", ParentID: &folder.ID})
	if err != nil {
		t.Fatal(err)
	}
	bm, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "Doc", URL: "https://doc.example/", ParentID: &sub.ID})
	if err != nil {
		t.Fatal(err)
	}
	// A bookmark cannot be a parent.
	if _, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "folder", Name: "bad", ParentID: &bm.ID}); err == nil {
		t.Error("bookmark accepted as parent")
	}
	// Cycle: moving Work under its own descendant Sub must fail.
	if _, err := bs.UpdateLibraryNode("", UpdateLibraryNodeInput{ID: folder.ID, Name: "Work", ParentID: &sub.ID}); err == nil {
		t.Error("folder cycle accepted")
	}
	// Rename + move to root works.
	renamed, err := bs.UpdateLibraryNode("", UpdateLibraryNodeInput{ID: sub.ID, Name: "Sub2"})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "Sub2" || renamed.ParentID != nil {
		t.Errorf("renamed = %+v", renamed)
	}
	// Reorder root level.
	nodes, err := bs.ListLibrary()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("library nodes = %d, want 3", len(nodes))
	}
	// Sub was moved to the root, so deleting Work removes only
	// Work itself; Sub2 and Doc (inside Sub2) survive.
	deleted, err := bs.DeleteLibraryNodes("", []string{folder.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	nodes, _ = bs.ListLibrary()
	if len(nodes) != 2 {
		t.Errorf("remaining = %+v", nodes)
	}
	// Deleting Sub2 now takes its subtree (Doc) with it.
	deleted, err = bs.DeleteLibraryNodes("", []string{renamed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("subtree deleted = %d, want 2", deleted)
	}
	nodes, _ = bs.ListLibrary()
	if len(nodes) != 0 {
		t.Errorf("remaining after subtree delete = %+v", nodes)
	}
}

func TestSyncToStoppedInstanceAndDedup(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "one")
	dir := profileDirOf(t, bs, inst)
	writeProfileBookmarks(t, dir, bookmarkFixture)

	folder, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "folder", Name: "Work"})
	if _, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "Doc", URL: "https://doc.example/", ParentID: &folder.ID}); err != nil {
		t.Fatal(err)
	}
	shared, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "Shared copy", URL: "https://shared.example/"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := bs.SyncBookmarks("", []string{folder.ID, shared.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	one := res.Instances[0]
	if one.Status != "success" || one.Added != 1 || one.Skipped != 1 {
		t.Fatalf("sync result = %+v", one)
	}
	file := readProfileBookmarks(t, dir)
	if file.CountBookmarks() != 2 { // Shared (existing) + Doc
		t.Fatalf("profile bookmarks = %d, want 2", file.CountBookmarks())
	}
	bar := file.Root("bookmark_bar")
	work := chromebookmarks.FindChildFolder(bar, "Work")
	if work == nil || !chromebookmarks.ChildHasURL(work, "https://doc.example/") {
		t.Fatal("synced folder/bookmark missing from bookmark bar")
	}
	// Second sync: everything is a duplicate now.
	res, err = bs.SyncBookmarks("", []string{folder.ID, shared.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	one = res.Instances[0]
	if one.Added != 0 || one.Skipped != 2 {
		t.Fatalf("second sync = %+v, want 0 added / 2 skipped", one)
	}
	if got := readProfileBookmarks(t, dir).CountBookmarks(); got != 2 {
		t.Fatalf("profile bookmarks after resync = %d, want 2", got)
	}
}

func TestSyncSkipsRunningInstance(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "runner")
	dir := profileDirOf(t, bs, inst)
	writeProfileBookmarks(t, dir, bookmarkFixture)
	if err := instSvc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	bm, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example/"})
	res, err := bs.SyncBookmarks("", []string{bm.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	one := res.Instances[0]
	if one.Status != "skipped" || !strings.Contains(one.Reason, "正在运行") {
		t.Fatalf("running sync = %+v", one)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "Bookmarks"))
	if string(raw) != bookmarkFixture {
		t.Error("running instance's bookmarks file was modified")
	}
}

func TestSearchEditDeleteAcrossInstances(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	a := makeInstance(t, instSvc, "a")
	b := makeInstance(t, instSvc, "b")
	dirA := profileDirOf(t, bs, a)
	dirB := profileDirOf(t, bs, b)
	writeProfileBookmarks(t, dirA, bookmarkFixture)
	writeProfileBookmarks(t, dirB, bookmarkFixture)

	// Both instances hold "Shared" — one aggregated group, count 2.
	search, err := bs.SearchInstanceBookmarks("", "shared", "bookmark")
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Groups) != 1 || search.Groups[0].InstanceCount != 2 || search.UncheckedInstances != 0 {
		t.Fatalf("search = %+v", search)
	}
	edited, err := bs.ApplySearchEdit("", search.Token, []string{search.Groups[0].Key}, "Renamed", "https://renamed.example/")
	if err != nil {
		t.Fatal(err)
	}
	if edited.UpdatedInstances != 2 || edited.AffectedNodes != 2 {
		t.Fatalf("edit = %+v", edited)
	}
	for _, dir := range []string{dirA, dirB} {
		file := readProfileBookmarks(t, dir)
		found := false
		file.WalkBookmarks(func(fb chromebookmarks.FlatBookmark) {
			if fb.Name == "Renamed" && fb.URL == "https://renamed.example/" {
				found = true
			}
		})
		if !found {
			t.Errorf("edit not applied in %s", dir)
		}
	}
	// The library was not touched by the profile edit.
	nodes, _ := bs.ListLibrary()
	if len(nodes) != 0 {
		t.Errorf("library changed by profile edit: %+v", nodes)
	}
	// Delete via a fresh search.
	search, err = bs.SearchInstanceBookmarks("", "renamed", "bookmark")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := bs.ApplySearchDelete("", search.Token, []string{search.Groups[0].Key})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.AffectedNodes != 2 {
		t.Fatalf("delete = %+v", deleted)
	}
	for _, dir := range []string{dirA, dirB} {
		if got := readProfileBookmarks(t, dir).CountBookmarks(); got != 0 {
			t.Errorf("bookmarks left in %s: %d", dir, got)
		}
	}
}

func TestSearchExcludesRunningInstances(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	a := makeInstance(t, instSvc, "stopped-one")
	b := makeInstance(t, instSvc, "running-one")
	dirA := profileDirOf(t, bs, a)
	dirB := profileDirOf(t, bs, b)
	writeProfileBookmarks(t, dirA, bookmarkFixture)
	writeProfileBookmarks(t, dirB, bookmarkFixture)
	if err := instSvc.StartInstance("", b.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	search, err := bs.SearchInstanceBookmarks("", "shared", "bookmark")
	if err != nil {
		t.Fatal(err)
	}
	if search.UncheckedInstances != 1 {
		t.Fatalf("unchecked = %d, want 1", search.UncheckedInstances)
	}
	if len(search.Groups) != 1 || search.Groups[0].InstanceCount != 1 {
		t.Fatalf("groups = %+v, want one group counting only the stopped instance", search.Groups)
	}
}

// nestedFolderFixture holds Outer > Inner > Deep in the bookmark
// bar, for parent+child batch operation tests.
const nestedFolderFixture = `{
  "checksum": "abc",
  "roots": {
    "bookmark_bar": {
      "children": [
        {"children": [
          {"children": [
            {"date_added": "13350000000000003", "date_last_used": "0", "guid": "g-8", "id": "8", "name": "Deep", "type": "url", "url": "https://deep.example/"}
          ], "date_added": "13349000000000000", "date_modified": "13350000000000003", "guid": "g-7", "id": "7", "name": "Inner", "type": "folder"}
        ], "date_added": "13349000000000000", "date_modified": "13350000000000003", "guid": "g-6", "id": "6", "name": "Outer", "type": "folder"}
      ],
      "date_added": "13300000000000000", "date_modified": "13350000000000003", "guid": "g-1", "id": "1", "name": "Bookmarks bar", "type": "folder"
    },
    "other": {"children": [], "date_added": "13300000000000000", "date_modified": "13300000000000000", "guid": "g-2", "id": "2", "name": "Other bookmarks", "type": "folder"},
    "synced": {"children": [], "date_added": "13300000000000000", "date_modified": "13300000000000000", "guid": "g-3", "id": "3", "name": "Mobile bookmarks", "type": "folder"}
  },
  "version": 1
}`

func TestMissingProfileDirIsNotOperable(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "no-profile-dir")
	dir := profileDirOf(t, bs, inst)
	// Neither a Local State nor any profile directory exists:
	// there is no profile to resolve, and nothing about the
	// instance may be treated as operable — and resolution must
	// not create directories or files to make it so.
	infos, err := bs.ListBookmarkInstances()
	if err != nil {
		t.Fatal(err)
	}
	if infos[0].BookmarkCount != -1 {
		t.Errorf("count = %d, want -1 (unknown)", infos[0].BookmarkCount)
	}
	bm, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example/"})
	res, err := bs.SyncBookmarks("", []string{bm.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instances[0].Status != "failed" {
		t.Errorf("sync to a profile-less instance = %+v, want failed", res.Instances[0])
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a profile directory was created for an unresolvable instance")
	}
	search, err := bs.SearchInstanceBookmarks("", "x", "bookmark")
	if err != nil {
		t.Fatal(err)
	}
	if search.UncheckedInstances != 1 || len(search.Groups) != 0 {
		t.Errorf("search = %+v, want the instance unchecked and no groups", search)
	}
}

// The reported defect: a freshly launched browser whose profile
// exists but has no Bookmarks file yet. The instance must be
// fully operable — count 0, searchable, and a sync target that
// gets its Bookmarks file created with the synced content.
func TestEmptyProfileIsFullyOperable(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "fresh")
	dir := freshProfileDir(t, bs, inst)

	infos, err := bs.ListBookmarkInstances()
	if err != nil {
		t.Fatal(err)
	}
	if infos[0].BookmarkCount != 0 {
		t.Errorf("count = %d, want 0 for an empty profile (not -1/unknown)", infos[0].BookmarkCount)
	}
	search, err := bs.SearchInstanceBookmarks("", "x", "bookmark")
	if err != nil {
		t.Fatal(err)
	}
	if search.UncheckedInstances != 0 || len(search.Groups) != 0 {
		t.Errorf("search = %+v, want the empty instance checked with no groups", search)
	}

	bm, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example/"})
	res, err := bs.SyncBookmarks("", []string{bm.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	one := res.Instances[0]
	if one.Status != "success" || one.Added != 1 {
		t.Fatalf("sync to empty profile = %+v, want success with 1 added", one)
	}
	file := readProfileBookmarks(t, dir)
	if file.CountBookmarks() != 1 || !chromebookmarks.ChildHasURL(file.Root("bookmark_bar"), "https://x.example/") {
		t.Fatal("the created Bookmarks file does not hold the synced bookmark")
	}
	// And the now-existing file dedups like any other.
	res, err = bs.SyncBookmarks("", []string{bm.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instances[0].Added != 0 || res.Instances[0].Skipped != 1 {
		t.Errorf("resync = %+v, want 0 added / 1 skipped", res.Instances[0])
	}
}

// Sync only ever merges into a target: a target that already has
// bookmarks keeps every one of them.
func TestSyncPreservesExistingTargetBookmarks(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "has-bookmarks")
	dir := profileDirOf(t, bs, inst)
	writeProfileBookmarks(t, dir, bookmarkFixtureTwo)

	bm, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example/"})
	res, err := bs.SyncBookmarks("", []string{bm.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instances[0].Status != "success" || res.Instances[0].Added != 1 {
		t.Fatalf("sync = %+v, want success with 1 added", res.Instances[0])
	}
	file := readProfileBookmarks(t, dir)
	bar := file.Root("bookmark_bar")
	if file.CountBookmarks() != 3 ||
		!chromebookmarks.ChildHasURL(bar, "https://a.example/") ||
		!chromebookmarks.ChildHasURL(bar, "https://b.example/") ||
		!chromebookmarks.ChildHasURL(bar, "https://x.example/") {
		t.Fatal("existing target bookmarks were not all preserved alongside the synced one")
	}
}

// A corrupt Bookmarks file is a different thing from a missing
// one: every entry point fails explicitly and the file is left
// byte-for-byte untouched.
func TestCorruptBookmarksStillFailsEverywhere(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "corrupt")
	dir := profileDirOf(t, bs, inst)
	const corrupt = "{not a bookmarks document"
	writeProfileBookmarks(t, dir, corrupt)

	infos, err := bs.ListBookmarkInstances()
	if err != nil {
		t.Fatal(err)
	}
	if infos[0].BookmarkCount != -1 {
		t.Errorf("count = %d, want -1 (unknown) for a corrupt file", infos[0].BookmarkCount)
	}
	bm, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example/"})
	res, err := bs.SyncBookmarks("", []string{bm.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instances[0].Status != "failed" {
		t.Errorf("sync to corrupt profile = %+v, want failed", res.Instances[0])
	}
	search, err := bs.SearchInstanceBookmarks("", "x", "bookmark")
	if err != nil {
		t.Fatal(err)
	}
	if search.UncheckedInstances != 1 {
		t.Errorf("search unchecked = %d, want 1 for the corrupt instance", search.UncheckedInstances)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Bookmarks"))
	if err != nil || string(raw) != corrupt {
		t.Error("the corrupt file was modified")
	}
}

// Each instance reads and writes its OWN resolved profile: an
// instance whose Local State names "Profile 1" uses that profile
// (not its Default, which also exists), and a sync to several
// instances lands in each one's own profile only.
func TestInstancesUseTheirOwnProfiles(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	one := makeInstance(t, instSvc, "one")
	two := makeInstance(t, instSvc, "two")

	// Instance one: last_used = Profile 1 (1 bookmark); its
	// Default holds a different document (2 bookmarks) that must
	// never be read or written for this instance.
	uddOne := filepath.Dir(profileDirOf(t, bs, one))
	if err := os.WriteFile(filepath.Join(uddOne, "Local State"), []byte(`{"profile":{"last_used":"Profile 1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profileOne := filepath.Join(uddOne, "Profile 1")
	writeProfileBookmarks(t, profileOne, bookmarkFixture)
	writeProfileBookmarks(t, filepath.Join(uddOne, "Default"), bookmarkFixtureTwo)
	// Instance two: plain Default fallback (1 bookmark).
	dirTwo := profileDirOf(t, bs, two)
	writeProfileBookmarks(t, dirTwo, bookmarkFixture)

	infos, err := bs.ListBookmarkInstances()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, info := range infos {
		counts[info.ID] = info.BookmarkCount
	}
	if counts[one.ID] != 1 || counts[two.ID] != 1 {
		t.Fatalf("counts = %v, want each instance counted in its own profile (1 and 1)", counts)
	}

	bm, _ := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example/"})
	res, err := bs.SyncBookmarks("", []string{bm.ID}, []string{one.ID, two.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Instances {
		if r.Status != "success" || r.Added != 1 {
			t.Fatalf("sync result = %+v, want success with 1 added for both", r)
		}
	}
	if got := readProfileBookmarks(t, profileOne).CountBookmarks(); got != 2 {
		t.Errorf("instance one's Profile 1 holds %d bookmarks, want 2", got)
	}
	def := readProfileBookmarks(t, filepath.Join(uddOne, "Default"))
	if def.CountBookmarks() != 2 || chromebookmarks.ChildHasURL(def.Root("bookmark_bar"), "https://x.example/") {
		t.Error("instance one's Default profile was read or written by mistake")
	}
	if got := readProfileBookmarks(t, dirTwo).CountBookmarks(); got != 2 {
		t.Errorf("instance two's Default holds %d bookmarks, want 2", got)
	}
}

func TestFolderParentChildBatchEditAndDelete(t *testing.T) {
	bs, instSvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, instSvc, "nested")
	dir := profileDirOf(t, bs, inst)
	writeProfileBookmarks(t, dir, nestedFolderFixture)

	search, err := bs.SearchInstanceBookmarks("", "", "folder")
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Groups) != 2 {
		t.Fatalf("folder groups = %+v, want Outer and Inner as separate groups", search.Groups)
	}
	keys := []string{search.Groups[0].Key, search.Groups[1].Key}

	// Edit: renaming the parent must not lose the child target.
	edited, err := bs.ApplySearchEdit("", search.Token, keys, "Renamed", "")
	if err != nil {
		t.Fatal(err)
	}
	if edited.AffectedNodes != 2 || edited.FailedNodes != 0 || edited.UpdatedInstances != 1 {
		t.Fatalf("parent+child edit = %+v", edited)
	}
	file := readProfileBookmarks(t, dir)
	var folderNames []string
	file.WalkFolders(func(f chromebookmarks.FlatFolder) { folderNames = append(folderNames, f.Name) })
	if len(folderNames) != 2 || folderNames[0] != "Renamed" || folderNames[1] != "Renamed" {
		t.Fatalf("folders after edit = %v", folderNames)
	}

	// Delete: the parent's subtree covers the child; the child
	// must not be reported failed.
	writeProfileBookmarks(t, dir, nestedFolderFixture)
	search, err = bs.SearchInstanceBookmarks("", "", "folder")
	if err != nil {
		t.Fatal(err)
	}
	keys = []string{search.Groups[0].Key, search.Groups[1].Key}
	deleted, err := bs.ApplySearchDelete("", search.Token, keys)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.AffectedNodes != 3 || deleted.FailedNodes != 0 || deleted.UpdatedInstances != 1 {
		t.Fatalf("parent+child delete = %+v, want 3 affected (Outer+Inner+Deep), 0 failed", deleted)
	}
	if got := readProfileBookmarks(t, dir).CountBookmarks(); got != 0 {
		t.Errorf("bookmarks left after folder delete: %d", got)
	}
}

func TestLibraryStoreRoundTrip(t *testing.T) {
	_, _, db := bookmarkTestServices(t)
	st := store.New(db)
	c := t.Context()
	n, err := st.CreateBookmarkNode(c, model.BookmarkNode{Kind: "folder", Name: "F"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBookmarkNode(c, model.BookmarkNode{Kind: "bookmark", Name: "B", URL: "https://b.example/", ParentID: &n.ID}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListBookmarkNodes(c)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	// Deleting a folder that still has children must fail at the
	// store level (no silent orphans); the service deletes
	// children first.
	if err := st.DeleteBookmarkNode(c, n.ID); err == nil {
		t.Error("folder with children deleted at store level")
	}
}

// The gate's "error state with no runtime record counts as
// stopped" rule must only apply when the runtime query PROVES
// there is no record (store.ErrNotFound). A runtime query that
// fails for any other reason must never be read as "no process":
// the instance stays unknown and nothing is read or written.
func TestRuntimeQueryErrorIsNotConfirmedStopped(t *testing.T) {
	bs, isvc, db := bookmarkTestServices(t)
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "gate-err", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	dir := profileDirOf(t, bs, inst)
	writeProfileBookmarks(t, dir, bookmarkFixture)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE browser_instances SET status = 'error' WHERE id = ?`, inst.ID); err != nil {
		t.Fatal(err)
	}
	// Control: error state + provably no runtime record = stopped.
	if _, _, gate, err := isvc.InstanceBookmarkAccess(inst.ID); err != nil || gate != BookmarkGateStopped {
		t.Fatalf("control gate = %q err %v, want stopped", gate, err)
	}
	// Break the runtime query itself (not a NotFound): the gate
	// must fall back to unknown, and sync must skip the instance
	// without touching its file.
	if _, err := db.SQL().ExecContext(ctx,
		`ALTER TABLE instance_runtime RENAME TO instance_runtime_broken`); err != nil {
		t.Fatal(err)
	}
	if _, _, gate, err := isvc.InstanceBookmarkAccess(inst.ID); err != nil || gate != BookmarkGateUnknown {
		t.Fatalf("broken-query gate = %q err %v, want unknown", gate, err)
	}
	node, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := bs.SyncBookmarks("", []string{node.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Instances) != 1 || res.Instances[0].Status != "skipped" {
		t.Fatalf("sync with broken runtime query = %+v, want one skipped instance", res.Instances)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Bookmarks"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != bookmarkFixture {
		t.Fatal("bookmarks file was modified despite the unconfirmable state")
	}
}

// The remaining row of the gate table: an instance in the
// persisted "error" state whose runtime record still EXISTS has
// a process that was never confirmed gone — the gate must stay
// unknown and sync must skip it without touching the file.
func TestErrorStateWithRuntimeRecordIsUnknown(t *testing.T) {
	bs, isvc, _ := bookmarkTestServices(t)
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "gate-err-record", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	dir := profileDirOf(t, bs, inst)
	writeProfileBookmarks(t, dir, bookmarkFixture)
	ctx := context.Background()
	if err := isvc.store.SetInstanceStatus(ctx, inst.ID, model.StatusError); err != nil {
		t.Fatal(err)
	}
	if err := isvc.store.UpsertInstanceRuntime(ctx, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 4242, ProcessCreatedAt: 1, Platform: "windows",
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
		StartedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, gate, err := isvc.InstanceBookmarkAccess(inst.ID); err != nil || gate != BookmarkGateUnknown {
		t.Fatalf("gate = %q err %v, want unknown", gate, err)
	}
	node, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{Kind: "bookmark", Name: "X", URL: "https://x.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := bs.SyncBookmarks("", []string{node.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Instances) != 1 || res.Instances[0].Status != "skipped" {
		t.Fatalf("sync = %+v, want one skipped instance", res.Instances)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Bookmarks"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != bookmarkFixture {
		t.Fatal("bookmarks file was modified for an unconfirmable instance")
	}
}
