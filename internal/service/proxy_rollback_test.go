package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Rollback-path tests for regenerateState and the port
// reassignment (§ reliability round). The core never really
// runs here: coreRunningOverride / reloadOverride /
// selectOverride script a "running core" whose reload and
// selection outcomes are deterministic, which is exactly what
// the production failure paths need exercised.

// runningCoreFixture returns a fixture with one binding whose
// core reads as running, reloads and selections scriptable, and
// a GeoIP database present (so ensureGeoIP stays a no-op).
func runningCoreFixture(t *testing.T) (*proxyFixture, string) {
	t.Helper()
	f, b := gateHealFixture(t)
	if err := os.MkdirAll(f.cfg.Layout.Mihomo, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.Layout.Mihomo, "geoip.metadb"), []byte("seed"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.selectOverride = func(ctx context.Context, group, node string) error { return nil }
	f.svc.reloadOverride = func(ctx context.Context) error { return nil }
	return f, b.InstanceID
}

func configBytes(t *testing.T, f *proxyFixture) []byte {
	t.Helper()
	data, err := os.ReadFile(f.svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Case 1: new config loads, selections apply — success, one
// reload, no rollback.
func TestRegenerateRollbackHappyPath(t *testing.T) {
	f, _ := runningCoreFixture(t)
	ctx := context.Background()
	reloads := 0
	f.svc.reloadOverride = func(ctx context.Context) error { reloads++; return nil }
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.regenerateState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1 (no rollback on success)", reloads)
	}
}

// Case 2: the core rejects the new config — the previous file
// is restored and the error says so.
func TestRegenerateRollbackReloadRejected(t *testing.T) {
	f, _ := runningCoreFixture(t)
	ctx := context.Background()
	// Produce the "previous" configuration with the core stopped.
	f.svc.coreRunningOverride = func() bool { return false }
	if err := f.svc.regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	before := configBytes(t, f)

	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.reloadOverride = func(ctx context.Context) error { return errors.New("core refused") }
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "已保留原有配置") {
		t.Fatalf("err = %v, want the reload-rejection error", err)
	}
	if got := configBytes(t, f); string(got) != string(before) {
		t.Fatal("a rejected reload must restore the previous configuration file")
	}
}

// Cases 3+4: the reload succeeds but applying selections
// fails; the recorded state is regenerated back (second
// reload), and the error reports the rollback honestly.
func TestRegenerateRollbackSelectionFailureRecovers(t *testing.T) {
	f, _ := runningCoreFixture(t)
	ctx := context.Background()
	// Baseline file from a stopped regeneration.
	f.svc.coreRunningOverride = func() bool { return false }
	if err := f.svc.regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	before := configBytes(t, f)
	f.svc.coreRunningOverride = func() bool { return true }

	reloads := 0
	f.svc.reloadOverride = func(ctx context.Context) error { reloads++; return nil }
	// Fail the first pass's selection attempt (the provider
	// read fails without a real core, so the attempt is the
	// REJECT select), succeed from the rollback pass on.
	selects := 0
	f.svc.selectOverride = func(ctx context.Context, group, node string) error {
		selects++
		if selects <= 1 {
			return errors.New("controller hiccup")
		}
		return nil
	}
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "已回滚到原配置") {
		t.Fatalf("err = %v, want the rolled-back error", err)
	}
	if !strings.Contains(err.Error(), "节点选择应用失败") {
		t.Fatalf("err = %v, want the original selection failure preserved", err)
	}
	if reloads != 2 {
		t.Fatalf("reloads = %d, want 2 (proposed + rollback)", reloads)
	}
	if got := configBytes(t, f); string(got) != string(before) {
		t.Fatal("after a successful rollback the configuration file must be the recorded state's")
	}
}

// Case 5: selections fail AND the rollback itself fails — the
// error keeps the original failure, names the rollback failure
// and advises a core restart; it never claims recovery.
func TestRegenerateRollbackSelectionFailureRollbackFails(t *testing.T) {
	f, _ := runningCoreFixture(t)
	ctx := context.Background()
	reloads := 0
	f.svc.reloadOverride = func(ctx context.Context) error {
		reloads++
		if reloads == 2 {
			return errors.New("core went away")
		}
		return nil
	}
	selects := 0
	f.svc.selectOverride = func(ctx context.Context, group, node string) error {
		selects++
		if selects <= 1 {
			return errors.New("controller hiccup")
		}
		return nil
	}
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"应用节点选择失败", "回滚原配置失败", "请重启代理核心恢复一致性"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "已回滚到原配置") {
		t.Fatalf("err = %v, must not claim a successful rollback", err)
	}
}

// §5: the reassignment's new listener never answers — the
// previous configuration is regenerated back, the recorded
// port is untouched, and the error reports the recovery.
func TestReassignPortProbeFailureRollsBack(t *testing.T) {
	f, instID := runningCoreFixture(t)
	ctx := context.Background()
	before, err := f.store.GetBinding(ctx, instID)
	if err != nil {
		t.Fatal(err)
	}
	reloads := 0
	f.svc.reloadOverride = func(ctx context.Context) error { reloads++; return nil }

	_, err = f.svc.ReassignInstancePort("op", instID)
	if err == nil || !strings.Contains(err.Error(), "已恢复原端口") {
		t.Fatalf("err = %v, want the probe-failure error reporting recovery", err)
	}
	if reloads != 2 {
		t.Fatalf("reloads = %d, want 2 (proposed + rollback)", reloads)
	}
	got, err := f.store.GetBinding(ctx, instID)
	if err != nil || got.Port != before.Port {
		t.Fatalf("binding = %+v, %v; the recorded port must stay %d", got, err, before.Port)
	}
}

// §5: probe failure AND rollback failure — both causes surface,
// the restart advice is given, and the recorded port is still
// untouched (the new port is never persisted on failure).
func TestReassignPortProbeFailureRollbackFails(t *testing.T) {
	f, instID := runningCoreFixture(t)
	ctx := context.Background()
	before, err := f.store.GetBinding(ctx, instID)
	if err != nil {
		t.Fatal(err)
	}
	reloads := 0
	f.svc.reloadOverride = func(ctx context.Context) error {
		reloads++
		if reloads == 2 {
			return errors.New("core went away")
		}
		return nil
	}
	_, err = f.svc.ReassignInstancePort("op", instID)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"监听未能就绪", "原配置恢复失败", "请重启代理核心恢复一致性"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "已恢复原端口") {
		t.Fatalf("err = %v, must not claim the old port was restored", err)
	}
	got, err := f.store.GetBinding(ctx, instID)
	if err != nil || got.Port != before.Port {
		t.Fatalf("binding = %+v, %v; the recorded port must stay %d", got, err, before.Port)
	}
}

// swapConfigToDir replaces the configuration file with a
// directory of the same name, so a later write-back of the
// previous configuration fails deterministically.
func swapConfigToDir(t *testing.T, f *proxyFixture) {
	t.Helper()
	p := f.svc.configPath()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o750); err != nil {
		t.Fatal(err)
	}
}

// Fix 1a: a configuration file that exists but cannot be read
// aborts the change before anything is replaced.
func TestRegeneratePrevConfigUnreadableAborts(t *testing.T) {
	f, _ := gateHealFixture(t)
	ctx := context.Background()
	_ = os.Remove(f.svc.configPath()) // the fixture's setup already wrote one
	if err := os.Mkdir(f.svc.configPath(), 0o750); err != nil {
		t.Fatal(err)
	}
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "读取现有核心配置失败") {
		t.Fatalf("err = %v, want the unreadable-previous-config abort", err)
	}
}

// Fix 1b: the core rejects the new configuration AND writing
// the previous file back fails — the error must combine both
// and must not claim the previous configuration was preserved.
func TestRegenerateRestoreWriteFailureReload(t *testing.T) {
	f, _ := runningCoreFixture(t)
	ctx := context.Background()
	f.svc.coreRunningOverride = func() bool { return false }
	if err := f.svc.regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.reloadOverride = func(ctx context.Context) error {
		swapConfigToDir(t, f)
		return errors.New("core refused")
	}
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"核心拒绝了新配置", "原配置恢复失败", "不一致"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "已保留原有配置") {
		t.Fatalf("err = %v, must not claim the previous configuration was preserved", err)
	}
}

// Fix 1c: same write-back discipline on the GeoIP preparation
// failure path.
func TestRegenerateRestoreWriteFailureGeoIP(t *testing.T) {
	f, _ := gateHealFixture(t) // rule-mode binding, no GeoIP database seeded
	ctx := context.Background()
	if err := f.svc.regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.geoipDownloadOverride = func(ctx context.Context, dir string) error {
		swapConfigToDir(t, f)
		return errors.New("network down")
	}
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"GeoIP", "原配置恢复失败", "不一致"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
}

// Fix 2a: adding a URL source whose provider refresh fails,
// with the compensation's database delete ALSO failing (the
// database is closed from inside the refresh seam): the error
// must report the cleanup failure and the source row must
// really still be there afterwards — no false "rolled back".
func TestAddURLSourceCompensationFailure(t *testing.T) {
	f, _ := runningCoreFixture(t)
	f.svc.refreshOverride = func(ctx context.Context, provider string) error {
		_ = f.db.Close() // every later store call fails
		return errors.New("core cannot load provider")
	}
	_, err := f.svc.AddURLSource("op", "坏源", testSubURL)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"回滚清理失败", "残留"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("err = %v, must not claim a clean rollback", err)
	}
	// The residue is real: reopen the database file and find
	// the source row still present.
	db2, err := database.Open(f.cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	sources, err := store.New(db2).ListProxySources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sources {
		if s.Name == "坏源" {
			found = true
		}
	}
	if !found {
		t.Fatal("the uncompensated source row should still exist")
	}
}

// Fix 2b: importing a file source whose reload fails, with the
// compensation's file delete ALSO failing (the copy is swapped
// for a non-empty directory from inside the reload seam): the
// row cleanup succeeds, the file residue remains, and the
// error reports the cleanup failure instead of a clean
// rollback.
func TestAddFileSourceCompensationFailure(t *testing.T) {
	f := testProxyFixture(t)
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "nodes.yaml")
	writeSourceYAML(t, srcFile, "NodeA=18101")
	f.svc.SourceFilePicker = func() (string, error) { return srcFile, nil }
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.selectOverride = func(ctx context.Context, group, node string) error { return nil }
	swapped := ""
	f.svc.reloadOverride = func(ctx context.Context) error {
		copies, _ := filepath.Glob(filepath.Join(f.svc.sourcesDir(), "*.yaml"))
		if len(copies) == 1 {
			swapped = copies[0]
			_ = os.Remove(swapped)
			_ = os.Mkdir(swapped, 0o750)
			_ = os.WriteFile(filepath.Join(swapped, "child"), []byte("x"), 0o640)
		}
		return errors.New("core refused")
	}
	_, err := f.svc.AddFileSource("op", "文件源")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"回滚清理失败", "文件副本"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("err = %v, must not claim a clean rollback", err)
	}
	// Real state: the database row was cleaned up...
	sources, err := f.svc.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v, want the row cleaned up", sources)
	}
	// ...but the file residue really remains.
	if swapped == "" {
		t.Fatal("the reload seam never saw the source copy")
	}
	if info, err := os.Stat(swapped); err != nil || !info.IsDir() {
		t.Fatalf("residue at %s: info=%v err=%v, want the non-empty directory left behind", swapped, info, err)
	}
}

// Fix 1d: with NO previous configuration file, a refused
// reload removes the just-written new file — the disk returns
// to the pre-change state (no file at all).
func TestRegenerateRollbackDeletesNewConfigWhenNoPrev(t *testing.T) {
	f, _ := gateHealFixture(t)
	ctx := context.Background()
	// GeoIP present so the flow reaches the reload; no config
	// file on disk when the change starts.
	if err := os.WriteFile(filepath.Join(f.cfg.Layout.Mihomo, "geoip.metadb"), []byte("seed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.svc.configPath()); err != nil {
		t.Fatal(err)
	}
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.reloadOverride = func(ctx context.Context) error { return errors.New("core refused") }
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "已删除新配置文件") {
		t.Fatalf("err = %v, want the refused-reload error noting the new file was removed", err)
	}
	if _, err := os.Stat(f.svc.configPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config file stat err = %v, want it removed by the rollback", err)
	}
}

// Fix 1e: same path, but removing the new file ALSO fails
// (the path was swapped for a non-empty directory inside the
// reload seam): the combined error reports both failures and
// claims neither preservation nor removal.
func TestRegenerateRollbackDeleteNewConfigFails(t *testing.T) {
	f, _ := gateHealFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.cfg.Layout.Mihomo, "geoip.metadb"), []byte("seed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.svc.configPath()); err != nil {
		t.Fatal(err)
	}
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.reloadOverride = func(ctx context.Context) error {
		p := f.svc.configPath()
		_ = os.Remove(p)
		_ = os.Mkdir(p, 0o750)
		_ = os.WriteFile(filepath.Join(p, "child"), []byte("x"), 0o640)
		return errors.New("core refused")
	}
	state, err := f.svc.loadConfigState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.regenerateState(ctx, state)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"核心拒绝了新配置", "原配置恢复失败", "不一致"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	for _, banned := range []string{"已删除新配置文件", "已保留原有配置"} {
		if strings.Contains(err.Error(), banned) {
			t.Fatalf("err = %v, must not claim %q", err, banned)
		}
	}
}

// Fix 2c: reference order — when the database row deletion
// fails, the vault entry the row still references must NOT be
// deleted. (Extends the scenario of
// TestAddURLSourceCompensationFailure with the vault check.)
func TestAddURLSourceRowDeleteFailureKeepsVaultEntry(t *testing.T) {
	f, _ := runningCoreFixture(t)
	f.svc.refreshOverride = func(ctx context.Context, provider string) error {
		_ = f.db.Close()
		return errors.New("core cannot load provider")
	}
	_, err := f.svc.AddURLSource("op", "顺序源", testSubURL)
	if err == nil || !strings.Contains(err.Error(), "删除来源记录失败") {
		t.Fatalf("err = %v, want the row-deletion failure reported", err)
	}
	db2, err := database.Open(f.cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	sources, err := store.New(db2).ListProxySources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, s := range sources {
		if s.Name == "顺序源" {
			ref = s.SecretRef
		}
	}
	if ref == "" {
		t.Fatal("the source row should still exist with its vault reference")
	}
	got, err := f.vault.Get(ref)
	if err != nil {
		t.Fatalf("vault entry must survive while the row references it: %v", err)
	}
	if string(got) != testSubURL {
		t.Fatalf("vault entry = %q, want the subscription URL intact", got)
	}
}

// Fix 2d: reference order for file sources — when the row
// deletion fails, the local file copy must NOT be deleted.
func TestAddFileSourceRowDeleteFailureKeepsFile(t *testing.T) {
	f := testProxyFixture(t)
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "nodes.yaml")
	writeSourceYAML(t, srcFile, "NodeA=18101")
	f.svc.SourceFilePicker = func() (string, error) { return srcFile, nil }
	f.svc.coreRunningOverride = func() bool { return true }
	f.svc.selectOverride = func(ctx context.Context, group, node string) error { return nil }
	f.svc.reloadOverride = func(ctx context.Context) error {
		_ = f.db.Close() // the compensation's row deletion will fail
		return errors.New("core refused")
	}
	_, err := f.svc.AddFileSource("op", "顺序文件源")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"回滚清理失败", "删除来源记录失败"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("err = %v, must not claim a clean rollback", err)
	}
	copies, _ := filepath.Glob(filepath.Join(f.svc.sourcesDir(), "*.yaml"))
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want the file copy kept while its row survives", copies)
	}
	if info, err := os.Stat(copies[0]); err != nil || info.IsDir() {
		t.Fatalf("copy stat: info=%v err=%v, want the regular file intact", info, err)
	}
}
