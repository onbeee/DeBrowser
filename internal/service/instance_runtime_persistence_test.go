package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/runner"
)

// Persistence failure handling for the "unknown" hold: the
// hold is durable only once the runtime record is written.
// These tests inject real database faults with SQLite
// triggers — writes to the named table fail while every other
// table keeps working — instead of mocking the store.

// blockWrites installs triggers that abort every INSERT and
// UPDATE on the named table until the test drops them.
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

func mustCreate(t *testing.T, svc *InstanceService, name string) model.Instance {
	t.Helper()
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: name, BrowserID: "reg-a"})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return inst
}

// When the runtime record cannot be written at the moment of
// an unconfirmed-exit hold, the abort path gets one last
// chance with the exact process object it holds; when that
// close also cannot confirm the exit, the failure must be
// reported as the severe error it is — never as an ordinary
// start failure — while this session still freezes the
// instance and no other instance is harmed.
func TestHoldSevereErrorWhenRuntimeWriteFails(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst := mustCreate(t, svc, "严重错误实例")
	blockWrites(t, db, "instance_runtime")
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
func TestUntrackedHoldSevereErrorWhenRuntimeWriteFails(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst := mustCreate(t, svc, "未跟踪严重错误")
	blockWrites(t, db, "instance_runtime")
	fl.launchErr = &runner.UntrackedChildError{PID: 777, ChildExited: false, Err: errors.New("no creation time")}

	err := svc.StartInstance("", inst.ID)
	if err == nil || !strings.Contains(err.Error(), "严重错误") {
		t.Fatalf("start error = %v, want a severe error report", err)
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown (memory mark)", got)
	}
}
