package service

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/runner"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Real-process proof for the post-verification abort on Windows
// (runs in CI): a launch whose identity verification fails —
// arranged by registering this test binary under a DIFFERENT
// textual form of its own path (long vs 8.3 short form), so the
// runner's inspected executable path can never textually match
// the registered one — must end with the exact child closed and
// confirmed gone, or, when the child has no window to close,
// with the instance held at "unknown" while the child provably
// keeps running.

var kernel32DLL = windows.NewLazySystemDLL("kernel32.dll")

func windowsPathForm(path, procName string) (string, bool) {
	proc := kernel32DLL.NewProc(procName)
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, 1024)
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(p16)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 || int(r) >= len(buf) {
		return "", false
	}
	out := windows.UTF16ToString(buf[:r])
	if out == "" || out == path {
		return "", false
	}
	return out, true
}

// mismatchedExeForm returns a textual form of exe that the
// runner's Inspect will NOT report (probed live), so a service
// launch registered under it fails executable verification.
func mismatchedExeForm(t *testing.T, exe string) string {
	t.Helper()
	t.Setenv("DEBROWSER_FAKE_BROWSER", "1")
	sys := runner.System()
	probe, err := sys.Launch(exe, "--user-data-dir="+t.TempDir())
	if err != nil {
		t.Fatalf("probe launch: %v", err)
	}
	info, ierr := sys.Inspect(probe.PID())
	if p, ferr := os.FindProcess(probe.PID()); ferr == nil {
		_ = p.Kill()
	}
	if ierr != nil {
		t.Fatalf("probe inspect: %v", ierr)
	}
	for _, procName := range []string{"GetLongPathNameW", "GetShortPathNameW"} {
		if form, ok := windowsPathForm(exe, procName); ok && !strings.EqualFold(form, info.ExecutablePath) {
			return form
		}
	}
	t.Skip("no alternate path form differs from the inspected executable path")
	return ""
}

func heartbeatSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func heartbeatAlive(path string) bool {
	s1 := heartbeatSize(path)
	time.Sleep(300 * time.Millisecond)
	return heartbeatSize(path) > s1
}

func heartbeatDead(path string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s1 := heartbeatSize(path)
		time.Sleep(250 * time.Millisecond)
		if heartbeatSize(path) == s1 && s1 > 0 {
			return true
		}
	}
	return false
}

// The child HAS a window: the abort's graceful close works, the
// exit is confirmed for real (the heartbeat stops), and the
// abort is an ordinary start failure — stopped, no record.
func TestRealStartAbortGracefulCloseExitsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process test skipped in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alt := mismatchedExeForm(t, exe)
	svc, cfg := testInstanceService(t)
	registerExecutable(t, cfg, "alt-browser", alt)
	hb := filepath.Join(t.TempDir(), "heartbeat.log")
	t.Setenv("DEBROWSER_FAKE_HEARTBEAT", hb)
	t.Setenv("DEBROWSER_FAKE_WINDOW", "1")

	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止真实关闭", BrowserID: "alt-browser"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start succeeded although the executable form cannot verify")
	}
	got, err := svc.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusStopped {
		t.Fatalf("status = %q, want stopped after a confirmed-exit abort", got.Status)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record held after a confirmed exit: %v", err)
	}
	if !heartbeatDead(hb) {
		t.Fatal("the aborted child is still running (heartbeat continues) — the close did not really exit it")
	}
}

// The child has NO window: WM_CLOSE has nowhere to go, the
// graceful close cannot succeed, and the exit cannot be
// confirmed. The instance must be held at "unknown" with a
// complete runtime record while the child provably keeps
// running (heartbeat continues); once the child is gone, a
// restart's reconciliation clears the hold.
func TestRealStartAbortNoWindowHoldsUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process test skipped in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alt := mismatchedExeForm(t, exe)
	svc, cfg := testInstanceService(t)
	registerExecutable(t, cfg, "alt-browser", alt)
	hb := filepath.Join(t.TempDir(), "heartbeat.log")
	t.Setenv("DEBROWSER_FAKE_HEARTBEAT", hb)
	// No DEBROWSER_FAKE_WINDOW: the child sleeps windowless.

	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止无窗口冻结", BrowserID: "alt-browser"})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.StartInstance("", inst.ID)
	if err == nil {
		t.Fatal("start succeeded although the executable form cannot verify")
	}
	if !strings.Contains(err.Error(), "无法确认") {
		t.Fatalf("error = %v, want it to state the exit could not be confirmed", err)
	}
	got, err := svc.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown while the child's exit is unconfirmed", got.Status)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rt, rerr := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if rerr != nil {
		t.Fatalf("runtime record missing for the held instance: %v", rerr)
	}
	if rt.ProcessCreatedAt == 0 {
		t.Fatal("held record has no creation time; this launch captured a complete identity")
	}
	if !heartbeatAlive(hb) {
		t.Fatal("heartbeat stopped — the child is gone, so the unknown hold has no basis")
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Error("second start allowed while the exit is unconfirmed")
	}
	// End the child from outside; a restart must then clear the
	// record and report the instance stopped.
	p, ferr := os.FindProcess(rt.PID)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if kerr := p.Kill(); kerr != nil {
		t.Fatalf("cleanup kill: %v", kerr)
	}
	fresh, err := NewInstanceService(db, cfg.Layout, slog.Default())
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	got, err = fresh.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusStopped {
		t.Fatalf("after restart with the child gone: status = %q, want stopped", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record not cleared after the child disappeared: %v", err)
	}
}
