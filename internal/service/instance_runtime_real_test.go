package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
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
	got, _ := svc.GetInstance(inst.ID)
	if got.Status != model.StatusRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}
	// The child must have received THIS instance's folder as its
	// user data directory.
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
	if !strings.Contains(string(rawArgs), "--user-data-dir="+absProfile) {
		t.Fatalf("fake browser args do not carry the instance folder:\n%s", strings.ReplaceAll(string(rawArgs), "\x00", " "))
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
