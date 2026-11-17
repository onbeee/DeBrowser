package service

import (
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

func profileDirOf(t *testing.T, svc *BookmarkService, inst model.Instance) string {
	t.Helper()
	_, dir, gate, err := svc.instances.InstanceBookmarkAccess(inst.ID)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if gate != BookmarkGateStopped {
		t.Fatalf("gate = %q, want stopped", gate)
	}
	return dir
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

func readProfileBookmarks(t *testing.T, dir string) *chromebookmarks.File {
	t.Helper()
	res, err := chromebookmarks.Load(dir)
	if err != nil {
		t.Fatalf("load profile bookmarks: %v", err)
	}
	return res.File
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
