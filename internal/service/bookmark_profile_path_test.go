package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/onbeee/DeBrowser/internal/chromebookmarks"
	"github.com/onbeee/DeBrowser/internal/model"
)

// End-to-end regression for the Chromium profile path fix: an
// instance's Bookmarks file lives in a profile subdirectory of
// its user data directory. With Local State naming "Profile 1",
// counting, searching and syncing must all operate on Profile 1 —
// and only Profile 1 — even though Default also has bookmarks.
func TestBookmarkEntriesUseLastUsedProfile(t *testing.T) {
	bs, isvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, isvc, "path-a")
	_, userDataDir, _, err := isvc.InstanceBookmarkAccess(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	defaultDir := filepath.Join(userDataDir, "Default")
	p1Dir := filepath.Join(userDataDir, "Profile 1")
	writeProfileBookmarks(t, defaultDir, bookmarkFixture) // 2 bookmarks in Default
	writeProfileBookmarks(t, p1Dir, nestedFolderFixture)  // different content in Profile 1
	if err := os.WriteFile(filepath.Join(userDataDir, "Local State"),
		[]byte(`{"profile":{"last_used":"Profile 1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	defaultBefore, _ := os.ReadFile(filepath.Join(defaultDir, "Bookmarks"))

	// 1. Counting reads Profile 1 (the fixture there holds its own
	// bookmark set, distinct from Default's two).
	infos, err := bs.ListBookmarkInstances()
	if err != nil {
		t.Fatal(err)
	}
	var count int = -2
	for _, info := range infos {
		if info.ID == inst.ID {
			count = info.BookmarkCount
		}
	}
	p1File, err := chromebookmarks.Load(p1Dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != p1File.CountBookmarks() {
		t.Fatalf("count = %d, want Profile 1's %d", count, p1File.CountBookmarks())
	}

	// 2. Search reads Profile 1: its "Deep" bookmark is found,
	// while Default's "Shared" bookmark is NOT — proof the search
	// did not touch the Default profile.
	deep, err := bs.SearchInstanceBookmarks("", "Deep", model.BookmarkKindBookmark)
	if err != nil {
		t.Fatal(err)
	}
	if deep.UncheckedInstances != 0 || len(deep.Groups) != 1 {
		t.Fatalf("search Deep: groups=%d unchecked=%d", len(deep.Groups), deep.UncheckedInstances)
	}
	shared, err := bs.SearchInstanceBookmarks("", "Shared", model.BookmarkKindBookmark)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.Groups) != 0 {
		t.Fatal("search found Default's bookmark — the wrong profile was searched")
	}

	// 3. Sync writes into Profile 1; Default stays byte-identical.
	node, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{
		Kind: model.BookmarkKindBookmark, Name: "Synced", URL: "https://synced.example/",
	})
	if err != nil {
		t.Fatal(err)
	}
	syncRes, err := bs.SyncBookmarks("", []string{node.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(syncRes.Instances) != 1 || syncRes.Instances[0].Status != "success" {
		t.Fatalf("sync result: %+v", syncRes.Instances)
	}
	after, err := chromebookmarks.Load(p1Dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.CountBookmarks() != p1File.CountBookmarks()+1 {
		t.Fatal("the synced bookmark did not land in Profile 1")
	}
	defaultAfter, _ := os.ReadFile(filepath.Join(defaultDir, "Bookmarks"))
	if string(defaultAfter) != string(defaultBefore) {
		t.Fatal("Default's Bookmarks file was modified")
	}
	if _, err := os.Stat(filepath.Join(p1Dir, "Bookmarks.debwbak")); err != nil {
		t.Fatalf("recovery copy missing next to Profile 1's Bookmarks: %v", err)
	}
}

// An instance whose user data directory has no resolvable profile
// (no Local State, no Default/Bookmarks) reports clean failures
// at every entry point instead of touching the wrong location.
func TestBookmarkEntriesFailWithoutResolvableProfile(t *testing.T) {
	bs, isvc, _ := bookmarkTestServices(t)
	inst := makeInstance(t, isvc, "path-b")
	infos, err := bs.ListBookmarkInstances()
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == inst.ID && info.BookmarkCount != -1 {
			t.Fatalf("count = %d, want -1 (unknown) with no profile", info.BookmarkCount)
		}
	}
	node, err := bs.CreateLibraryNode("", CreateLibraryNodeInput{
		Kind: model.BookmarkKindBookmark, Name: "X", URL: "https://x.example/",
	})
	if err != nil {
		t.Fatal(err)
	}
	syncRes, err := bs.SyncBookmarks("", []string{node.ID}, []string{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(syncRes.Instances) != 1 || syncRes.Instances[0].Status != "failed" {
		t.Fatalf("sync without a profile must fail per-instance: %+v", syncRes.Instances)
	}
	res, err := bs.SearchInstanceBookmarks("", "", model.BookmarkKindBookmark)
	if err != nil {
		t.Fatal(err)
	}
	if res.UncheckedInstances < 1 {
		t.Fatal("search must count the profile-less instance as unchecked")
	}
}
