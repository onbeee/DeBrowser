package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/runner"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Protection persistence (review round 3): the "unknown" hold
// for a browser whose exit could not be confirmed must be
// DURABLE. These tests inject real database faults with SQLite
// triggers — writes to the named table fail while every other
// table keeps working — instead of mocking the store.

// blockWrites installs triggers that abort every INSERT and
// UPDATE on the named table until unblockWrites drops them.
func blockWrites(t *testing.T, db *database.DB, table string) {
	t.Helper()
	for _, ev := range []string{"INSERT", "UPDATE"} {
		name := "block_" + table + "_" + strings.ToLower(ev)
		stmt := "CREATE TRIGGER " + name + " BEFORE " + ev + " ON " + table +
			" BEGIN SELECT RAISE(ABORT, 'injected write failure'); END"
		if _, err := db.SQL().ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("install trigger on %s: %v", table, err)
		}
	}
}

func unblockWrites(t *testing.T, db *database.DB, table string) {
	t.Helper()
	for _, ev := range []string{"INSERT", "UPDATE"} {
		name := "block_" + table + "_" + strings.ToLower(ev)
		if _, err := db.SQL().ExecContext(context.Background(), "DROP TRIGGER IF EXISTS "+name); err != nil {
			t.Fatalf("drop trigger on %s: %v", table, err)
		}
	}
}

func mustCreate(t *testing.T, svc *InstanceService, name string) model.Instance {
	t.Helper()
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: name, BrowserID: "reg-a"})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return inst
}

// fallbackHold drives the unconfirmed-exit hold (post-launch
// verification fails on a foreign command line; the graceful
// close fails while the child stays alive) with instance_runtime
// writes blocked at the database level, and returns the scene
// once the hold stands on the protection journal alone.
func fallbackHold(t *testing.T) (*InstanceService, config.Config, *database.DB, *fakeLauncher, model.Instance, *fakeProcess) {
	t.Helper()
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst := mustCreate(t, svc, "保护实例")
	blockWrites(t, db, "instance_runtime")
	fl.spoofDir = true
	fl.closeErrOnLaunch = errors.New("no window to close")
	err := svc.StartInstance("", inst.ID)
	if err == nil || !strings.Contains(err.Error(), "无法确认") {
		t.Fatalf("start error = %v, want the unconfirmed-exit hold", err)
	}
	if strings.Contains(err.Error(), "严重错误") {
		t.Fatalf("start error = %v, want a journal-backed hold, not a severe error", err)
	}
	return svc, cfg, db, fl, inst, fl.lastProc
}

func restartService(t *testing.T, db *database.DB, cfg config.Config, fl *fakeLauncher) *InstanceService {
	t.Helper()
	svc, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("restart service: %v", err)
	}
	return svc
}

// With the runtime record unwritable, the hold must fall back
// to the protection journal: same identity, instance unknown,
// no severe error, and unrelated database work unaffected.
func TestHoldFallsBackToProtectionJournal(t *testing.T) {
	svc, _, db, _, inst, child := fallbackHold(t)

	if got := statusOf(t, svc, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown", got)
	}
	prot, err := store.New(db).GetInstanceProtection(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("protection journal entry missing: %v", err)
	}
	if prot.PID != child.pid || prot.ProcessCreatedAt != child.created || prot.ProcessCreatedAt == 0 {
		t.Fatalf("journal = pid %d created %d, want the child's pid %d and real creation time %d",
			prot.PID, prot.ProcessCreatedAt, child.pid, child.created)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record err = %v, want ErrNotFound (its writes were blocked)", err)
	}
	if child.closes != 1 {
		t.Errorf("child close attempts = %d, want exactly 1 (no retry when the journal holds)", child.closes)
	}
	// Other database work is unaffected by the blocked table.
	other := mustCreate(t, svc, "其他实例")
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{ID: other.ID, Name: "其他实例改名"}); err != nil {
		t.Errorf("update of an unrelated instance failed: %v", err)
	}
	if err := svc.DeleteInstance("", other.ID); err != nil {
		t.Errorf("delete of an unrelated instance failed: %v", err)
	}
}

// The journal-backed hold survives a restart: with the process
// alive but its command line unreadable, the fresh service must
// keep the instance at unknown — never reset it to stopped —
// with every risky operation still frozen.
func TestProtectionJournalSurvivesRestartUnknown(t *testing.T) {
	_, cfg, db, fl, inst, child := fallbackHold(t)

	fl.mu.Lock()
	info := fl.infos[child.pid]
	info.HasCommandLine = false
	fl.infos[child.pid] = info
	fl.mu.Unlock()

	svc2 := restartService(t, db, cfg, fl)
	if got := statusOf(t, svc2, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status after restart = %q, want unknown (never stopped)", got)
	}
	if _, err := store.New(db).GetInstanceProtection(t.Context(), inst.ID); err != nil {
		t.Fatalf("protection journal lost across restart: %v", err)
	}
	if err := svc2.StartInstance("", inst.ID); err == nil {
		t.Error("start allowed on a journal-held instance after restart")
	}
	if err := svc2.DeleteInstance("", inst.ID); err == nil {
		t.Error("delete allowed on a journal-held instance after restart")
	}
	if err := svc2.DeleteInstanceAndProfile("", inst.ID); err == nil {
		t.Error("delete-with-profile allowed on a journal-held instance after restart")
	}
}

// Once the held process is proven gone, the restart clears the
// journal and the instance is an ordinary stopped instance.
func TestProtectionJournalClearsWhenProcessGone(t *testing.T) {
	_, cfg, db, fl, inst, child := fallbackHold(t)

	child.exit(0)
	svc2 := restartService(t, db, cfg, fl)
	if got := statusOf(t, svc2, inst.ID); got != model.StatusStopped {
		t.Fatalf("status after restart = %q, want stopped", got)
	}
	if _, err := store.New(db).GetInstanceProtection(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("protection journal err = %v, want ErrNotFound after proven exit", err)
	}
	if err := svc2.DeleteInstance("", inst.ID); err != nil {
		t.Errorf("delete after protection cleared: %v", err)
	}
}

// A journal-held process whose identity fully verifies at
// restart is promoted: while the runtime table is still
// blocked the instance stays held; once writes work again the
// next restart writes the real record, drops the journal, and
// re-adopts the process.
func TestProtectionJournalPromotesOnVerifiedRestart(t *testing.T) {
	_, cfg, db, fl, inst, child := fallbackHold(t)

	fl.mu.Lock()
	info := fl.infos[child.pid]
	info.CommandLine = "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst)
	fl.infos[child.pid] = info
	fl.mu.Unlock()

	svc2 := restartService(t, db, cfg, fl)
	if got := statusOf(t, svc2, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status with promotion blocked = %q, want unknown", got)
	}
	if _, err := store.New(db).GetInstanceProtection(t.Context(), inst.ID); err != nil {
		t.Fatalf("journal dropped although promotion failed: %v", err)
	}

	unblockWrites(t, db, "instance_runtime")
	svc3 := restartService(t, db, cfg, fl)
	if got := statusOf(t, svc3, inst.ID); got != model.StatusRunning {
		t.Fatalf("status after promotion = %q, want running", got)
	}
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if err != nil || rt.PID != child.pid {
		t.Fatalf("promoted runtime record = %+v (err %v), want pid %d", rt, err, child.pid)
	}
	if _, err := store.New(db).GetInstanceProtection(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("journal err = %v, want ErrNotFound after promotion", err)
	}
	child.closeErr = nil // the scene's close failure no longer applies
	if err := svc3.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop promoted instance: %v", err)
	}
}

// The store-level stale-status reset must never touch a
// journal-held instance: seeded running with a journal entry
// and no runtime record, it stays running on disk (the journal
// path judges it), not silently reset to stopped.
func TestReconcileStatusesSkipsProtectionJournal(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst := mustCreate(t, svc, "对账保护")
	st := store.New(db)
	prot := model.InstanceProtection{
		InstanceID: inst.ID, PID: 4242, ProcessCreatedAt: 424200,
		ExecutablePath: "fake-browser.exe",
		UserDataDir:    inst.UserDataDir, MarkedAt: "2026-11-16T00:00:00Z",
	}
	if err := st.UpsertInstanceProtection(t.Context(), prot); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	if err := st.SetInstanceStatus(t.Context(), inst.ID, model.StatusRunning); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	// The recorded process exists but its command line cannot
	// be read: verification can neither confirm nor disprove it.
	fl.mu.Lock()
	fl.infos[4242] = runner.Info{PID: 4242, CreatedAt: 424200, ExecutablePath: "fake-browser.exe", HasCommandLine: false}
	fl.procs[4242] = &fakeProcess{pid: 4242, created: 424200, exited: make(chan struct{})}
	fl.mu.Unlock()

	svc2 := restartService(t, db, cfg, fl)
	row, err := st.GetInstance(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("get instance row: %v", err)
	}
	if row.Status != model.StatusRunning {
		t.Fatalf("persisted status = %q, want running (the stale-status reset must skip journal-held instances)", row.Status)
	}
	if got := statusOf(t, svc2, inst.ID); got != model.StatusUnknown {
		t.Fatalf("effective status = %q, want unknown", got)
	}
}

// When BOTH the runtime record and the journal fail to write,
// the abort path gets one last chance with the exact process
// object it holds; when that close also cannot confirm the
// exit, the failure must be reported as the severe error it
// is — never as an ordinary start failure — while this session
// still freezes the instance and no other instance is harmed.
func TestHoldSevereErrorWhenAllProtectionWritesFail(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst := mustCreate(t, svc, "严重错误实例")
	blockWrites(t, db, "instance_runtime")
	blockWrites(t, db, "instance_protection")
	fl.spoofDir = true
	fl.closeErrOnLaunch = errors.New("no window to close")

	err := svc.StartInstance("", inst.ID)
	if err == nil || !strings.Contains(err.Error(), "严重错误") {
		t.Fatalf("start error = %v, want a severe error report", err)
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown (this session's memory mark still holds)", got)
	}
	if fl.lastProc.closes != 2 {
		t.Errorf("child close attempts = %d, want 2 (the initial attempt plus the persistence-failure retry)", fl.lastProc.closes)
	}
	other := mustCreate(t, svc, "未受影响")
	if err := svc.DeleteInstance("", other.ID); err != nil {
		t.Errorf("unrelated instance harmed by the severe failure: %v", err)
	}
}

// The untracked-launch path has no process object left to
// retry with: if its hold cannot be persisted either, the
// severe error is reported directly.
func TestUntrackedHoldSevereErrorWhenProtectionWritesFail(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst := mustCreate(t, svc, "未跟踪严重错误")
	blockWrites(t, db, "instance_runtime")
	blockWrites(t, db, "instance_protection")
	fl.launchErr = &runner.UntrackedChildError{PID: 777, ChildExited: false, Err: errors.New("no creation time")}

	err := svc.StartInstance("", inst.ID)
	if err == nil || !strings.Contains(err.Error(), "严重错误") {
		t.Fatalf("start error = %v, want a severe error report", err)
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown (memory mark)", got)
	}
}
