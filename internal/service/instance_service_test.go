package service

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

func testInstanceService(t *testing.T) (*InstanceService, config.Config) {
	t.Helper()
	cfg, err := config.Load(func(key string) string {
		if key == config.EnvAppRoot {
			return t.TempDir()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc, err := NewInstanceService(db, cfg.Layout, slog.Default())
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, cfg
}

func TestInstanceCreateDefaultsAndValidation(t *testing.T) {
	svc, _ := testInstanceService(t)

	if _, err := svc.CreateInstance(CreateInstanceInput{Name: "  "}); err == nil {
		t.Error("empty name accepted, want error")
	}
	created, err := svc.CreateInstance(CreateInstanceInput{Name: "工作号"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if created.UserDataDir != "profiles/"+created.ID {
		t.Errorf("default user_data_dir = %q, want profiles/<id>", created.UserDataDir)
	}
	if created.Status != model.StatusStopped {
		t.Errorf("status = %q, want stopped", created.Status)
	}
	// Duplicate name -> friendly Chinese error.
	if _, err := svc.CreateInstance(CreateInstanceInput{Name: "工作号"}); err == nil ||
		!strings.Contains(err.Error(), "实例名称已存在") {
		t.Errorf("duplicate name error = %v, want 实例名称已存在", err)
	}
	// Duplicate explicit dir -> friendly error.
	if _, err := svc.CreateInstance(CreateInstanceInput{Name: "x", UserDataDir: created.UserDataDir}); err == nil ||
		!strings.Contains(err.Error(), "数据目录已被") {
		t.Errorf("duplicate dir error = %v, want 数据目录已被占用", err)
	}
	// Unsafe explicit dirs are refused.
	for _, bad := range []string{"/abs/path", "../escape", "profiles/../secret", "profiles", "elsewhere/x"} {
		if _, err := svc.CreateInstance(CreateInstanceInput{Name: "bad", UserDataDir: bad}); err == nil {
			t.Errorf("unsafe dir %q accepted, want error", bad)
		}
	}
	// Missing proxy is refused before insert.
	ghost := "no-such-proxy"
	if _, err := svc.CreateInstance(CreateInstanceInput{Name: "ghost", ProxyID: &ghost}); err == nil ||
		!strings.Contains(err.Error(), "所选代理不存在") {
		t.Errorf("ghost proxy error = %v, want 所选代理不存在", err)
	}
	list, err := svc.ListInstances()
	if err != nil || len(list) != 1 {
		t.Errorf("list = %d (err %v), want exactly the one valid instance", len(list), err)
	}
}

func TestInstanceUpdateAndDeleteMetadataOnly(t *testing.T) {
	svc, cfg := testInstanceService(t)
	created, err := svc.CreateInstance(CreateInstanceInput{Name: "可删"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate real profile data on disk.
	profileDir := filepath.Join(cfg.Layout.Data, filepath.FromSlash(created.UserDataDir))
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(profileDir, "Preferences")
	if err := os.WriteFile(marker, []byte("user-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, err := svc.UpdateInstance(UpdateInstanceInput{
		ID: created.ID, Name: "改名后", StartURL: "https://example.com",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "改名后" || updated.StartURL != "https://example.com" {
		t.Errorf("updated = %+v", updated)
	}
	if _, err := svc.UpdateInstance(UpdateInstanceInput{ID: "missing", Name: "x"}); err == nil {
		t.Error("update of missing instance accepted, want error")
	}

	// Metadata-only delete must keep the profile files.
	if err := svc.DeleteInstance(created.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := svc.GetInstance(created.ID); err == nil {
		t.Error("instance still readable after metadata delete")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("profile file was removed by metadata-only delete: %v", err)
	}
	if err := svc.DeleteInstance(created.ID); err == nil {
		t.Error("second metadata delete accepted, want 实例不存在")
	}
}

func TestDeleteInstanceAndProfileSafety(t *testing.T) {
	svc, cfg := testInstanceService(t)
	created, err := svc.CreateInstance(CreateInstanceInput{Name: "连删"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	profileDir := filepath.Join(cfg.Layout.Data, filepath.FromSlash(created.UserDataDir))
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "Cookies"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unrelated directory inside profiles/ must survive.
	other := filepath.Join(cfg.Layout.Profiles, "someone-else")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteInstanceAndProfile(created.ID); err != nil {
		t.Fatalf("DeleteInstanceAndProfile: %v", err)
	}
	if _, err := os.Stat(profileDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("profile dir still exists after purge (err=%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unrelated profile dir was removed: %v", err)
	}
	if _, err := svc.GetInstance(created.ID); err == nil {
		t.Error("metadata still present after purge")
	}
}

func TestDeleteProfileRefusesRunningAndEscapes(t *testing.T) {
	svc, cfg := testInstanceService(t)
	// Seed rows directly through the store to create states the
	// service API itself would not produce.
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	st := store.New(db)
	running, err := st.CreateInstance(t.Context(), model.Instance{
		Name: "运行中", UserDataDir: "profiles/running", Status: model.StatusRunning,
	})
	if err != nil {
		t.Fatalf("seed running: %v", err)
	}
	if err := svc.DeleteInstanceAndProfile(running.ID); err == nil ||
		!strings.Contains(err.Error(), "正在运行") {
		t.Errorf("purge of running instance error = %v, want refusal", err)
	}
	// A stored path escaping profiles/ must be refused before any
	// filesystem effect.
	esc, err := st.CreateInstance(t.Context(), model.Instance{
		Name: "逃逸", UserDataDir: "../outside",
	})
	if err != nil {
		t.Fatalf("seed escape: %v", err)
	}
	if err := svc.DeleteInstanceAndProfile(esc.ID); err == nil ||
		!strings.Contains(err.Error(), "不在 profiles 目录之内") {
		t.Errorf("purge of escaping path error = %v, want refusal", err)
	}
}

func TestStartupReconcilesRunningToStopped(t *testing.T) {
	svc, cfg := testInstanceService(t)
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	st := store.New(db)
	stale, err := st.CreateInstance(t.Context(), model.Instance{
		Name: "残留", UserDataDir: "profiles/stale", Status: model.StatusRunning,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A fresh service (as at app startup) reconciles the stale state.
	fresh, err := NewInstanceService(db, cfg.Layout, slog.Default())
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	_ = svc
	got, err := fresh.GetInstance(stale.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusStopped {
		t.Errorf("status after startup reconciliation = %q, want stopped", got.Status)
	}
}
