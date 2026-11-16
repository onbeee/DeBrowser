package service

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/runner"
	"github.com/onbeee/DeBrowser/internal/store"
)

// End-to-end runtime proof with REAL OS processes: the service
// launches this very test binary as a fake browser (see
// TestMain). The child records its arguments, stays alive, and is
// then closed through the runner's graceful path — SIGTERM on
// Unix, WM_CLOSE to its window on Windows. A second launch is
// killed externally, simulating the user (or a crash) ending the
// browser without DeBrowser.

func registerExecutable(t *testing.T, cfg config.Config, id, path string) {
	t.Helper()
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := store.New(db).CreateExternalBrowser(t.Context(), model.ExternalBrowser{
		ID: id, Name: "self", ExecutablePath: path,
	}); err != nil {
		t.Fatalf("register executable: %v", err)
	}
}

func TestRealBrowserLaunchGracefulCloseAndExternalKill(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process test skipped in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc, cfg := testInstanceService(t) // real system launcher
	registerExecutable(t, cfg, "self-browser", exe)

	argsFile := filepath.Join(t.TempDir(), "fake-browser-args.txt")
	t.Setenv("DEBROWSER_FAKE_BROWSER", "1")
	t.Setenv("DEBROWSER_FAKE_ARGS_FILE", argsFile)
	if runtime.GOOS == "windows" {
		t.Setenv("DEBROWSER_FAKE_WINDOW", "1")
	}

	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "真实进程", BrowserID: "self-browser"})
	if err != nil {
		t.Fatal(err)
	}
	absProfile := filepath.Join(cfg.Layout.Profiles, "P-1")

	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start real browser: %v", err)
	}
	// Never leak the child on a failure path: a leftover fake
	// browser keeps the test binary file locked on Windows and
	// poisons the rest of the run.
	defer func() { _ = svc.StopInstance("", inst.ID) }()
	got, _ := svc.GetInstance(inst.ID)
	if got.Status != model.StatusRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}
	// The child must have received THIS instance's folder as its
	// user data directory. Compare directory identity, not raw
	// strings: the service launches with the fully resolved
	// (long-form) path, while t.TempDir may spell the same
	// directory in 8.3 short form on Windows ("RUNNER~1").
	deadline := time.Now().Add(5 * time.Second)
	var rawArgs []byte
	for time.Now().Before(deadline) {
		if rawArgs, err = os.ReadFile(argsFile); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("fake browser never recorded its arguments: %v", err)
	}
	recorded := string(rawArgs)
	const marker = "--user-data-dir="
	idx := strings.Index(recorded, marker)
	if idx < 0 {
		t.Fatalf("fake browser args lack %s:\n%s", marker, strings.ReplaceAll(recorded, "\x00", " "))
	}
	value := recorded[idx+len(marker):]
	if end := strings.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	wantInfo, err := os.Stat(absProfile)
	if err != nil {
		t.Fatalf("stat instance folder: %v", err)
	}
	gotInfo, err := os.Stat(value)
	if err != nil {
		t.Fatalf("stat recorded user data dir %q: %v", value, err)
	}
	if !os.SameFile(wantInfo, gotInfo) {
		t.Fatalf("fake browser user data dir = %q, want the instance folder %q", value, absProfile)
	}

	// Graceful close through the service.
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("graceful stop of real browser: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)

	// Launch again and kill the browser externally: DeBrowser
	// must notice the exit on its own and stop calling the
	// instance running.
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("second start: %v", err)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	_ = db.Close()
	if err != nil {
		t.Fatalf("runtime record: %v", err)
	}
	proc, err := os.FindProcess(rt.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("external kill: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := svc.GetInstance(inst.ID)
		if got.Status != model.StatusRunning {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ = svc.GetInstance(inst.ID)
	t.Fatalf("status after external kill = %q, want not running", got.Status)
}

// --- Real-process proof for the post-verification abort ---

// exeMismatchLauncher wraps the real system launcher and makes
// Inspect report a different executable path than the one the
// process was really launched from. The child process, its
// close and its exit are all real; only the reported path is
// perturbed, which deterministically drives the launch into
// the verification-failure abort on every platform. (Registering
// the binary under an alternate textual form of its path does
// NOT work: the OS reports the path in the form it was launched
// with, so it always matches the registration.)
type exeMismatchLauncher struct {
	runner.Launcher
}

func (l exeMismatchLauncher) Inspect(pid int) (runner.Info, error) {
	info, err := l.Launcher.Inspect(pid)
	if err == nil {
		info.ExecutablePath = "not-the-registered-browser.exe"
	}
	return info, err
}

func realServiceWithLauncher(t *testing.T, launcher runner.Launcher) (*InstanceService, config.Config) {
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
	svc, err := newInstanceService(db, cfg.Layout, slog.Default(), launcher)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, cfg
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
		if s1 > 0 && heartbeatSize(path) == s1 {
			return true
		}
	}
	return false
}

// killHeldChild is a test backstop: if a runtime record exists
// for the instance, end that process from outside so a failed
// assertion can never leak a fake browser (a leftover child
// keeps the test binary locked on Windows).
func killHeldChild(t *testing.T, cfg config.Config, instanceID string) {
	t.Helper()
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), instanceID)
	if err != nil {
		return
	}
	if p, ferr := os.FindProcess(rt.PID); ferr == nil {
		_ = p.Kill()
	}
}

// Verification fails for a real child that CAN be closed
// gracefully (window on Windows, SIGTERM disposition on Unix):
// the abort confirms the real exit — the heartbeat stops — and
// stays an ordinary start failure: stopped, no record.
func TestRealStartAbortGracefulCloseExitsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process test skipped in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc, cfg := realServiceWithLauncher(t, exeMismatchLauncher{Launcher: runner.System()})
	registerExecutable(t, cfg, "self-browser", exe)
	hb := filepath.Join(t.TempDir(), "heartbeat.log")
	t.Setenv("DEBROWSER_FAKE_BROWSER", "1")
	t.Setenv("DEBROWSER_FAKE_HEARTBEAT", hb)
	if runtime.GOOS == "windows" {
		t.Setenv("DEBROWSER_FAKE_WINDOW", "1")
	}
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止真实关闭", BrowserID: "self-browser"})
	if err != nil {
		t.Fatal(err)
	}
	defer killHeldChild(t, cfg, inst.ID)
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start succeeded although the inspected executable cannot match the registration")
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

// Verification fails for a real child that does NOT react to
// the graceful close (windowless on Windows, so WM_CLOSE has
// nowhere to go; ignoring SIGTERM on Unix): its exit cannot be
// confirmed, so the instance is held at "unknown" with a
// complete runtime record while the child provably keeps
// running (heartbeat continues). Once the child is gone, a
// restart's reconciliation clears the hold.
func TestRealStartAbortUnconfirmedExitHoldsUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process test skipped in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc, cfg := realServiceWithLauncher(t, exeMismatchLauncher{Launcher: runner.System()})
	registerExecutable(t, cfg, "self-browser", exe)
	hb := filepath.Join(t.TempDir(), "heartbeat.log")
	t.Setenv("DEBROWSER_FAKE_BROWSER", "1")
	t.Setenv("DEBROWSER_FAKE_HEARTBEAT", hb)
	if runtime.GOOS != "windows" {
		t.Setenv("DEBROWSER_FAKE_IGNORE_TERM", "1")
	}
	// On Windows no DEBROWSER_FAKE_WINDOW: the child is
	// windowless, so the graceful close cannot reach it.
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止无法确认退出", BrowserID: "self-browser"})
	if err != nil {
		t.Fatal(err)
	}
	defer killHeldChild(t, cfg, inst.ID)
	err = svc.StartInstance("", inst.ID)
	if err == nil {
		t.Fatal("start succeeded although the inspected executable cannot match the registration")
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
	// Release this test's own handle at once: on Windows a
	// terminated process stays openable — and thus inspectable,
	// with an unreadable command line — while ANY handle to it
	// exists, and FindProcess's handle counts. (The launcher's
	// own handle is closed by its watcher once the exit is
	// observed.)
	_ = p.Release()
	// "Gone" must be established, not assumed: a just-killed
	// child can briefly linger as an unreaped zombie whose
	// command line is already unreadable — correctly judged
	// unknown — so wait until inspection reports it gone.
	sys := runner.System()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ierr := sys.Inspect(rt.PID); errors.Is(ierr, runner.ErrProcessGone) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child still inspectable 3s after being killed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The hold kept the process watched in this session, so the
	// unified watcher settles it here first: the record is
	// deleted and the abnormal (killed) exit lands as "error".
	waitStatus(t, svc, inst.ID, model.StatusError)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record not cleared after the child disappeared: %v", err)
	}
	// A restart then finds nothing to re-attach and nothing
	// left at unknown: the settled state stands.
	fresh, err := NewInstanceService(db, cfg.Layout, slog.Default())
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	got, err = fresh.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == model.StatusUnknown || got.Status == model.StatusRunning {
		t.Fatalf("after restart with the child gone: status = %q, want a settled state", got.Status)
	}
}
