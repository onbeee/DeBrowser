package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stage 9B instance-file rules: sequential default names that are
// never reused, custom creation, binding of retained folders,
// rename/swap on edit, and the one-to-one binding between
// instances and folders.

func TestDefaultProfileSequenceNeverReusesNumbers(t *testing.T) {
	svc, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)
	create := func(name string) string {
		t.Helper()
		inst, err := svc.CreateInstance("", CreateInstanceInput{Name: name, BrowserID: browserID})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return inst.ID + "|" + inst.UserDataDir
	}
	a := create("甲")
	b := create("乙")
	if !strings.HasSuffix(a, "profiles/P-1") || !strings.HasSuffix(b, "profiles/P-2") {
		t.Fatalf("sequence = %q, %q; want P-1, P-2", a, b)
	}
	// Deleting instance A (metadata only) keeps its folder; the
	// next default must still move forward, not recycle P-1.
	aID := strings.Split(a, "|")[0]
	if err := svc.DeleteInstance("", aID); err != nil {
		t.Fatal(err)
	}
	c := create("丙")
	if !strings.HasSuffix(c, "profiles/P-3") {
		t.Fatalf("after delete, sequence = %q; want P-3 (no reuse)", c)
	}
	// Deleting B WITH its folder retires P-2 as well: the number
	// is never handed out again even though the folder is gone.
	bID := strings.Split(b, "|")[0]
	if err := svc.DeleteInstanceAndProfile("", bID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.Profiles, "P-2")); !os.IsNotExist(err) {
		t.Fatalf("P-2 folder should be deleted, stat err = %v", err)
	}
	d := create("丁")
	if !strings.HasSuffix(d, "profiles/P-4") {
		t.Fatalf("after purge, sequence = %q; want P-4 (no reuse)", d)
	}
}

func TestCreateCustomAndExistingProfiles(t *testing.T) {
	svc, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)

	// Custom mode creates and binds a new folder.
	custom, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "工作号", BrowserID: browserID, ProfileMode: ProfileModeCustom, ProfileName: "工作账号",
	})
	if err != nil {
		t.Fatalf("custom create: %v", err)
	}
	if custom.UserDataDir != "profiles/工作账号" {
		t.Fatalf("custom dir = %q", custom.UserDataDir)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.Profiles, "工作账号")); err != nil {
		t.Fatalf("custom folder not created: %v", err)
	}
	// The same folder name cannot be created twice.
	if _, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "重复", BrowserID: browserID, ProfileMode: ProfileModeCustom, ProfileName: "工作账号",
	}); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("duplicate custom error = %v", err)
	}

	// A retained, unbound folder with data can be bound as-is:
	// no copy, content untouched.
	retained := filepath.Join(cfg.Layout.Profiles, "保留号")
	if err := os.MkdirAll(retained, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retained, "Preferences"), []byte("keep-me"), 0o644); err != nil {
		t.Fatal(err)
	}
	avail, err := svc.ListAvailableProfiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range avail {
		if p.Name == "保留号" {
			found = true
		}
		if p.Name == "工作账号" {
			t.Error("bound folder offered as available")
		}
	}
	if !found {
		t.Fatalf("retained unbound folder not offered: %+v", avail)
	}
	bound, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "绑定保留", BrowserID: browserID, ProfileMode: ProfileModeExisting, ProfileName: "保留号",
	})
	if err != nil {
		t.Fatalf("existing create: %v", err)
	}
	if bound.UserDataDir != "profiles/保留号" {
		t.Fatalf("existing dir = %q", bound.UserDataDir)
	}
	data, err := os.ReadFile(filepath.Join(retained, "Preferences"))
	if err != nil || string(data) != "keep-me" {
		t.Fatalf("retained folder content disturbed: %q, %v", data, err)
	}
	// Now bound, it can no longer be selected by anyone.
	if _, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "再绑", BrowserID: browserID, ProfileMode: ProfileModeExisting, ProfileName: "保留号",
	}); err == nil {
		t.Fatal("binding an already-bound folder was accepted")
	}
	// A folder that does not exist cannot be selected either.
	if _, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "不存在", BrowserID: browserID, ProfileMode: ProfileModeExisting, ProfileName: "没影",
	}); err == nil {
		t.Fatal("binding a nonexistent folder was accepted")
	}
}

func TestUpdateRenameProfileKeepsData(t *testing.T) {
	svc, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)
	inst, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "改名前", BrowserID: browserID, ProfileMode: ProfileModeCustom, ProfileName: "旧文件夹",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(cfg.Layout.Profiles, "旧文件夹")
	if err := os.WriteFile(filepath.Join(oldDir, "Cookies"), []byte("session"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Renaming the instance must NOT rename its folder...
	renamed, err := svc.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: "改名后"})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.UserDataDir != "profiles/旧文件夹" {
		t.Fatalf("instance rename changed the folder binding: %q", renamed.UserDataDir)
	}
	// ...but the edit form's rename action renames the folder
	// itself, data included, and updates the binding.
	updated, err := svc.UpdateInstance("", UpdateInstanceInput{
		ID: inst.ID, Name: "改名后", ProfileAction: ProfileActionRename, ProfileName: "新文件夹",
	})
	if err != nil {
		t.Fatalf("rename folder: %v", err)
	}
	if updated.UserDataDir != "profiles/新文件夹" {
		t.Fatalf("binding after rename = %q", updated.UserDataDir)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("old folder still exists after rename (err=%v)", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.Layout.Profiles, "新文件夹", "Cookies"))
	if err != nil || string(data) != "session" {
		t.Errorf("folder data lost in rename: %q, %v", data, err)
	}
	// Renaming onto an existing folder is refused.
	if err := os.MkdirAll(filepath.Join(cfg.Layout.Profiles, "占用"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{
		ID: inst.ID, Name: "改名后", ProfileAction: ProfileActionRename, ProfileName: "占用",
	}); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Errorf("rename onto existing folder error = %v", err)
	}
	if got, _ := svc.GetInstance(inst.ID); got.UserDataDir != "profiles/新文件夹" {
		t.Errorf("binding moved by refused rename: %q", got.UserDataDir)
	}
}

func TestUpdateSwapProfileKeepsOldFolder(t *testing.T) {
	svc, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)
	a, err := svc.CreateInstance("", CreateInstanceInput{Name: "甲", BrowserID: browserID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateInstance("", CreateInstanceInput{Name: "乙", BrowserID: browserID})
	if err != nil {
		t.Fatal(err)
	}
	// a -> P-1, b -> P-2. An unbound P-3 folder exists on disk.
	p3 := filepath.Join(cfg.Layout.Profiles, "P-3")
	if err := os.MkdirAll(p3, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p3, "marker"), []byte("p3"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdateInstance("", UpdateInstanceInput{
		ID: a.ID, Name: "甲", ProfileAction: ProfileActionSwap, ProfileName: "P-3",
	})
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if updated.UserDataDir != "profiles/P-3" {
		t.Fatalf("binding after swap = %q", updated.UserDataDir)
	}
	// The old folder stays on disk, unbound, and becomes
	// selectable; the other instance is untouched.
	if _, err := os.Stat(filepath.Join(cfg.Layout.Profiles, "P-1")); err != nil {
		t.Errorf("old folder was removed by swap: %v", err)
	}
	if got, _ := svc.GetInstance(b.ID); got.UserDataDir != "profiles/P-2" {
		t.Errorf("other instance disturbed by swap: %q", got.UserDataDir)
	}
	avail, err := svc.ListAvailableProfiles()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range avail {
		names[p.Name] = true
	}
	if !names["P-1"] || names["P-3"] || names["P-2"] {
		t.Errorf("available after swap = %v, want only P-1", names)
	}
	// Swapping onto another instance's folder is refused.
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{
		ID: a.ID, Name: "甲", ProfileAction: ProfileActionSwap, ProfileName: "P-2",
	}); err == nil {
		t.Error("swap onto a bound folder was accepted")
	}
}

func TestMetadataDeleteFreesFolderForSelection(t *testing.T) {
	svc, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "临时", BrowserID: browserID})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	// The retained folder (P-1) is selectable again...
	avail, err := svc.ListAvailableProfiles()
	if err != nil || len(avail) != 1 || avail[0].Name != "P-1" {
		t.Fatalf("available after metadata delete = %+v, %v", avail, err)
	}
	// ...and a new instance can bind it.
	next, err := svc.CreateInstance("", CreateInstanceInput{
		Name: "接手", BrowserID: browserID, ProfileMode: ProfileModeExisting, ProfileName: "P-1",
	})
	if err != nil {
		t.Fatalf("rebind retained folder: %v", err)
	}
	if next.UserDataDir != "profiles/P-1" {
		t.Fatalf("rebound dir = %q", next.UserDataDir)
	}
}

func TestUpdateNameUniqueness(t *testing.T) {
	svc, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)
	if _, err := svc.CreateInstance("", CreateInstanceInput{Name: "甲", BrowserID: browserID}); err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateInstance("", CreateInstanceInput{Name: "乙", BrowserID: browserID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{ID: b.ID, Name: "甲"}); err == nil ||
		!strings.Contains(err.Error(), "实例名称已存在") {
		t.Errorf("rename to a taken name error = %v", err)
	}
	// Keeping one's own name is fine.
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{ID: b.ID, Name: "乙"}); err != nil {
		t.Errorf("keeping own name refused: %v", err)
	}
}
