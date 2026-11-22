package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
