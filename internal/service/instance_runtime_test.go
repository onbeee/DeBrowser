package service

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/runner"
	"github.com/onbeee/DeBrowser/internal/store"
)

// A scriptable fake Launcher/Process pair: the service's runtime
// logic (state machine, verification, gates, reconciliation) is
// tested against these deterministic doubles, and against real
// child processes separately (instance_runtime_real_test.go).

type fakeProcess struct {
	pid      int
	created  int64
	exited   chan struct{}
	exitOnce sync.Once
	code     int
	closeErr error
	closes   int
	// closeExits makes CloseGracefully end the process even
	// while returning closeErr (a close error racing a real
	// exit). closeDelay makes the close take that long before
	// answering (a close that outlives the caller's patience).
	closeExits bool
	closeDelay time.Duration
}

func (p *fakeProcess) PID() int                { return p.pid }
func (p *fakeProcess) CreatedAt() int64        { return p.created }
func (p *fakeProcess) Exited() <-chan struct{} { return p.exited }
func (p *fakeProcess) ExitCode() int           { return p.code }
func (p *fakeProcess) exit(code int) {
	p.exitOnce.Do(func() { p.code = code; close(p.exited) })
}
func (p *fakeProcess) CloseGracefully(time.Duration) error {
	p.closes++
	if p.closeDelay > 0 {
		time.Sleep(p.closeDelay)
	}
	if p.closeExits {
		p.exit(0)
		return p.closeErr
	}
	if p.closeErr != nil {
		return p.closeErr
	}
	p.exit(0)
	return nil
}

type fakeLauncher struct {
	mu           sync.Mutex
	seq          int
	procs        map[int]*fakeProcess
	infos        map[int]runner.Info
	lastArgs     map[int][]string
	launchErr    error
	exitOnLaunch *int // when set, launched processes exit immediately with this code
	spoofDir     bool // Inspect reports a command line for a different folder
	noCmdline    bool // Inspect cannot read the command line
	// cmdlineOverride, when set, replaces the command line
	// Inspect reports for launched processes (verbatim).
	cmdlineOverride string
	// Close behavior copied onto every launched process.
	closeErrOnLaunch   error
	closeExitsOnLaunch bool
	closeDelayOnLaunch time.Duration
	lastProc           *fakeProcess
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		procs:    map[int]*fakeProcess{},
		infos:    map[int]runner.Info{},
		lastArgs: map[int][]string{},
	}
}

func (l *fakeLauncher) Launch(executable string, args ...string) (runner.Process, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.launchErr != nil {
		return nil, l.launchErr
	}
	l.seq++
	pid := 1000 + l.seq
	p := &fakeProcess{pid: pid, created: int64(pid * 100), exited: make(chan struct{}),
		closeErr: l.closeErrOnLaunch, closeExits: l.closeExitsOnLaunch, closeDelay: l.closeDelayOnLaunch}
	cmdline := executable + " " + strings.Join(args, " ")
	if l.spoofDir {
		cmdline = executable + ` --user-data-dir=C:\elsewhere\other-profile`
	}
	if l.cmdlineOverride != "" {
		cmdline = l.cmdlineOverride
	}
	l.procs[pid] = p
	l.infos[pid] = runner.Info{
		PID: pid, CreatedAt: p.created, ExecutablePath: executable,
		CommandLine: cmdline, HasCommandLine: !l.noCmdline,
	}
	l.lastArgs[pid] = append([]string{}, args...)
	l.lastProc = p
	if l.exitOnLaunch != nil {
		p.exit(*l.exitOnLaunch)
	}
	return p, nil
}

func (l *fakeLauncher) Inspect(pid int) (runner.Info, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.procs[pid]
	if !ok {
		return runner.Info{}, runner.ErrProcessGone
	}
	select {
	case <-p.exited:
		return runner.Info{}, runner.ErrProcessGone
	default:
	}
	return l.infos[pid], nil
}

func (l *fakeLauncher) Open(pid int, createdAt int64) (runner.Process, error) {
	info, err := l.Inspect(pid)
	if err != nil {
		return nil, err
	}
	if info.CreatedAt != createdAt {
		return nil, runner.ErrIdentityMismatch
	}
	return l.procs[pid], nil
}

// plant registers a live fake process the launcher "knows", for
// reconciliation scenarios (as if a previous DeBrowser run had
// launched it).
func (l *fakeLauncher) plant(pid int, info runner.Info) *fakeProcess {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := &fakeProcess{pid: pid, created: info.CreatedAt, exited: make(chan struct{})}
	l.procs[pid] = p
	l.infos[pid] = info
	return p
}

// shortenStopConfirm shrinks the stop operation's wait for the
// watcher's cleanup signal, for tests whose close never lands.
func shortenStopConfirm(t *testing.T) func() {
	t.Helper()
	orig := stopConfirmTimeout
	stopConfirmTimeout = 100 * time.Millisecond
	return func() { stopConfirmTimeout = orig }
}

func testRuntimeService(t *testing.T, launcher runner.Launcher) (*InstanceService, config.Config, *database.DB) {
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
	if err := cfg.Layout.Ensure(); err != nil {
		t.Fatalf("layout ensure: %v", err)
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
	return svc, cfg, db
}

func registerFakeBrowser(t *testing.T, db *database.DB, id, path string) {
	t.Helper()
	if _, err := store.New(db).CreateExternalBrowser(t.Context(), model.ExternalBrowser{
		ID: id, Name: "fake", ExecutablePath: path,
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func waitStatus(t *testing.T, svc *InstanceService, id string, want model.InstanceStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := svc.GetInstance(id)
		if err == nil && got.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := svc.GetInstance(id)
	t.Fatalf("status of %s = %q, want %q", id, got.Status, want)
}

func TestStartStopLifecycleWithFakeBrowser(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "运行实例", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("op-start", inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, _ := svc.GetInstance(inst.ID)
	if got.Status != model.StatusRunning {
		t.Fatalf("status after start = %q, want running", got.Status)
	}
	// The browser was launched with THIS instance's folder.
	args := fl.lastArgs[fl.lastProc.pid]
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--user-data-dir=") || !strings.Contains(joined, "P-1") {
		t.Fatalf("launch args = %v, want --user-data-dir pointing at P-1", args)
	}
	// A runtime identity record exists.
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if err != nil || rt.PID != fl.lastProc.pid {
		t.Fatalf("runtime record = %+v, %v", rt, err)
	}
	// A repeated start of a running instance is idempotent:
	// it succeeds without launching a second process.
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("double start: %v", err)
	}
	if fl.seq != 1 {
		t.Fatalf("launches = %d, want 1 (no second process)", fl.seq)
	}
	// Stop closes gracefully and clears the record.
	if err := svc.StopInstance("op-stop", inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record after stop: %v", err)
	}
	if err := svc.StopInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "未在运行") {
		t.Fatalf("second stop error = %v", err)
	}
}

func TestStartFailures(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	st := store.New(db)

	// No kernel association at all.
	bare, err := st.CreateInstance(t.Context(), model.Instance{Name: "无内核", UserDataDir: "profiles/bare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", bare.ID); err == nil || !strings.Contains(err.Error(), "尚未关联") {
		t.Errorf("start without kernel error = %v", err)
	}
	// Association to a registration that no longer exists.
	ghost, err := st.CreateInstance(t.Context(), model.Instance{
		Name: "幽灵", UserDataDir: "profiles/ghost", KernelID: "ghost-reg", KernelName: "ghost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", ghost.ID); err == nil || !strings.Contains(err.Error(), "登记不存在") {
		t.Errorf("start with ghost kernel error = %v", err)
	}
	// The OS refuses to start the executable.
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "起不来", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.launchErr = errors.New("cannot execute")
	if err := svc.StartInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "浏览器启动失败") {
		t.Errorf("launch failure error = %v", err)
	}
	fl.launchErr = nil
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	// The browser exits immediately after launch.
	code := 2
	fl.exitOnLaunch = &code
	if err := svc.StartInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "立即退出") {
		t.Errorf("immediate exit error = %v", err)
	}
	fl.exitOnLaunch = nil
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := st.GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runtime record survived a failed start: %v", err)
	}
}

func TestStartRejectsForeignProfileProcess(t *testing.T) {
	fl := newFakeLauncher()
	fl.spoofDir = true // the "launched" process reports another folder in its command line
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "核查", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "未使用该实例的实例文件") {
		t.Fatalf("spoofed start error = %v", err)
	}
	// The unverifiable process was asked to close (it was started
	// by this very operation) and nothing was recorded as running.
	if fl.lastProc.closes != 1 {
		t.Errorf("aborted process close attempts = %d, want 1", fl.lastProc.closes)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
}

func TestProcessExitIsDetected(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	st := store.New(db)

	clean, err := svc.CreateInstance("", CreateInstanceInput{Name: "自关", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", clean.ID); err != nil {
		t.Fatal(err)
	}
	cleanProc := fl.lastProc
	// The user closes the browser window directly: exit code 0.
	cleanProc.exit(0)
	waitStatus(t, svc, clean.ID, model.StatusStopped)
	if _, err := st.GetInstanceRuntime(t.Context(), clean.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runtime record survived process exit: %v", err)
	}

	crash, err := svc.CreateInstance("", CreateInstanceInput{Name: "崩溃", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", crash.ID); err != nil {
		t.Fatal(err)
	}
	fl.lastProc.exit(9)
	waitStatus(t, svc, crash.ID, model.StatusError)
}

func TestStopFailureKeepsInstanceRunning(t *testing.T) {
	restoreConfirm := shortenStopConfirm(t)
	defer restoreConfirm()
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "关不掉", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	fl.lastProc.closeErr = errors.New("window did not close")
	if err := svc.StopInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "关闭未完成") {
		t.Fatalf("refused close error = %v", err)
	}
	got, _ := svc.GetInstance(inst.ID)
	if got.Status != model.StatusRunning {
		t.Fatalf("status after failed close = %q, want running (no false success)", got.Status)
	}
	// Once the browser cooperates, the close succeeds. The
	// shortened confirm window exists only to keep the FAILED
	// stop fast; the success path gets the production window —
	// demanding that the watcher's exit cleanup (a goroutine
	// plus database writes) land inside 100 ms is a scheduling
	// assumption no loaded CI runner can guarantee, and it is
	// not part of the behavior under test.
	restoreConfirm()
	fl.lastProc.closeErr = nil
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
}

// exitOnInspectLauncher makes the launched process die while
// the start flow is verifying it: Inspect captures the identity
// first, then the process exits, then the (valid) identity is
// reported as if nothing had happened.
type exitOnInspectLauncher struct {
	*fakeLauncher
}

func (l *exitOnInspectLauncher) Inspect(pid int) (runner.Info, error) {
	info, err := l.fakeLauncher.Inspect(pid)
	if err == nil {
		l.mu.Lock()
		p := l.procs[pid]
		l.mu.Unlock()
		if p != nil {
			p.exit(0)
		}
	}
	return info, err
}

// A process that exits during the start's identity verification
// must never be persisted as running: the launch is an ordinary
// failure and no runtime record remains.
func TestStartProcessExitsDuringVerification(t *testing.T) {
	base := newFakeLauncher()
	fl := &exitOnInspectLauncher{fakeLauncher: base}
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "核验时退出", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start of a process that died during verification succeeded")
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record written for a process that died during verification: %v", err)
	}
}

// A stop returns only after the watcher has completed the
// unified exit cleanup: while the close is still in flight the
// instance reads "stopping" (never a premature "stopped"), and
// by the time the stop returns, the runtime record is already
// deleted by the watcher.
func TestStopWaitsForWatcherCleanup(t *testing.T) {
	fl := newFakeLauncher()
	fl.closeDelayOnLaunch = 400 * time.Millisecond
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "统一清理", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.StopInstance("", inst.ID) }()
	time.Sleep(100 * time.Millisecond)
	if got, _ := svc.GetInstance(inst.ID); got.Status != model.StatusStopping {
		t.Fatalf("status during an in-flight close = %q, want stopping", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Fatalf("runtime record deleted before the exit landed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The stop returned only after the watcher's cleanup: the
	// record is already gone and the status settled.
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record still present when the stop returned: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
}

// A close request that reports an error while the process
// actually exited is a successful close: the confirmed exit,
// settled by the watcher, decides — not the request's error.
func TestStopCloseErrorButExitedSucceeds(t *testing.T) {
	fl := newFakeLauncher()
	fl.closeErrOnLaunch = errors.New("close raced the exit")
	fl.closeExitsOnLaunch = true
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "报错已退", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop whose close errored but the process exited: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the raced close: %v", err)
	}
}

// A failed stop changes nothing about ownership: the watcher
// keeps the process, and when the browser later exits on its
// own the watcher settles it as usual.
func TestFailedStopLeavesWatcherInCharge(t *testing.T) {
	defer shortenStopConfirm(t)()
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "关不掉后自关", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	fl.lastProc.closeErr = errors.New("window did not close")
	if err := svc.StopInstance("", inst.ID); err == nil {
		t.Fatal("stop of an unclosable browser succeeded")
	}
	if got, _ := svc.GetInstance(inst.ID); got.Status != model.StatusRunning {
		t.Fatalf("status after failed stop = %q, want running", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Fatalf("runtime record lost after a failed stop: %v", err)
	}
	fl.lastProc.exit(0)
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the self-exit after a failed stop: %v", err)
	}
}

// While the watcher is settling an exited process, the
// instance cannot be restarted: a new start would persist a
// fresh record and running status that the old watcher's by-ID
// cleanup would then delete and overwrite. The cleanup window
// is held open deterministically through watchSettleHook.
func TestWatcherCleanupBlocksRestart(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "清理期禁启动", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	watchSettleHook = func(id string) {
		if id == inst.ID {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	defer func() { watchSettleHook = nil }()

	fl.lastProc.exit(0)
	<-entered // the watcher observed the exit; its cleanup is held open

	if got, _ := svc.GetInstance(inst.ID); got.Status != model.StatusStopping {
		t.Fatalf("status during exit cleanup = %q, want stopping", got.Status)
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start during exit cleanup succeeded")
	}
	if fl.seq != 1 {
		t.Fatalf("launches during cleanup = %d, want 1 (no second process)", fl.seq)
	}
	close(release)
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the cleanup: %v", err)
	}
	// After the cleanup, a fresh start works — and the old
	// watcher, long finished, cannot touch the new process's
	// record or status.
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("restart after cleanup: %v", err)
	}
	if fl.seq != 2 {
		t.Fatalf("launches after cleanup = %d, want 2", fl.seq)
	}
	waitStatus(t, svc, inst.ID, model.StatusRunning)
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if err != nil || rt.PID != fl.lastProc.pid {
		t.Fatalf("new runtime record = %+v, %v", rt, err)
	}
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop of the restarted instance: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the restarted instance's stop: %v", err)
	}
}

// A stop that times out hands the process back to the watcher
// completely: a later abnormal self-exit is judged on its own
// and lands as "error", not as a belated successful close.
func TestStopTimeoutThenAbnormalSelfExitIsError(t *testing.T) {
	defer shortenStopConfirm(t)()
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "超时后异常自退", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	fl.lastProc.closeErr = errors.New("window did not close")
	if err := svc.StopInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "关闭未完成") {
		t.Fatalf("timed-out stop error = %v", err)
	}
	if got, _ := svc.GetInstance(inst.ID); got.Status != model.StatusRunning {
		t.Fatalf("status after the timed-out stop = %q, want running", got.Status)
	}
	svc.rtMu.Lock()
	_, transientLeft := svc.transient[inst.ID]
	svc.rtMu.Unlock()
	if transientLeft {
		t.Fatal("timed-out stop left its transition behind")
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Fatalf("runtime record lost after the timed-out stop: %v", err)
	}
	fl.lastProc.exit(7)
	waitStatus(t, svc, inst.ID, model.StatusError)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the abnormal self-exit: %v", err)
	}
}

// When a stop's confirm wait times out in the same moment the
// watcher observes the exit, the watcher's already-captured
// outcome stands: the stop must not reset stopAsked under it.
// The cleanup is held open through watchSettleHook so the
// ordering is deterministic.
func TestStopTimeoutRacingWatcherCleanupKeepsWatcherOutcome(t *testing.T) {
	defer shortenStopConfirm(t)()
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "关闭与退出竞态", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	proc := fl.lastProc
	proc.closeErr = errors.New("window did not close")
	proc.closeDelay = 300 * time.Millisecond

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	watchSettleHook = func(id string) {
		if id == inst.ID {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	defer func() { watchSettleHook = nil }()

	stopDone := make(chan error, 1)
	go func() { stopDone <- svc.StopInstance("", inst.ID) }()
	// Wait until the stop is actually in flight (its close
	// request is sleeping inside CloseGracefully), then the
	// browser exits abnormally on its own.
	deadline := time.Now().Add(2 * time.Second)
	for {
		svc.rtMu.Lock()
		st := svc.transient[inst.ID]
		svc.rtMu.Unlock()
		if st == model.StatusStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop never entered the stopping state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	proc.exit(3)
	<-entered // the watcher observed the exit with stopAsked set and is mid-cleanup
	if err := <-stopDone; err == nil {
		t.Fatal("stop reported success before the watcher cleanup landed")
	}
	// The timed-out stop must not have lifted the watcher's
	// cleanup guard: a restart is still refused and creates no
	// second process while the cleanup is held open.
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("restart allowed during the watcher cleanup after a timed-out stop")
	}
	if fl.seq != 1 {
		t.Fatalf("launches = %d, want 1 (no second process)", fl.seq)
	}
	close(release)
	// The exit answered a close request, so the watcher's
	// captured outcome — stopped, despite the abnormal code —
	// is the final state, and the record is gone exactly once.
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the raced cleanup: %v", err)
	}
	// Once the cleanup has completed, a restart works and its
	// record is the new process's own.
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("restart after the raced cleanup: %v", err)
	}
	if fl.seq != 2 {
		t.Fatalf("launches after the raced cleanup = %d, want 2", fl.seq)
	}
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if err != nil || rt.PID != fl.lastProc.pid {
		t.Fatalf("runtime record after the restart = %+v, %v", rt, err)
	}
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop of the restarted instance: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusStopped)
}

// One instance's browser exiting on its own never disturbs
// another running instance.
func TestOneInstanceSelfExitLeavesOtherRunning(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	a, err := svc.CreateInstance("", CreateInstanceInput{Name: "甲留", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateInstance("", CreateInstanceInput{Name: "乙退", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", a.ID); err != nil {
		t.Fatal(err)
	}
	procA := fl.lastProc
	if err := svc.StartInstance("", b.ID); err != nil {
		t.Fatal(err)
	}
	fl.lastProc.exit(0)
	waitStatus(t, svc, b.ID, model.StatusStopped)
	got, _ := svc.GetInstance(a.ID)
	if got.Status != model.StatusRunning {
		t.Fatalf("A status after B self-exited = %q, want running", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), a.ID); err != nil {
		t.Fatalf("A's runtime record disturbed by B's exit: %v", err)
	}
	_ = procA
	if err := svc.StopInstance("", a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRunningInstanceGates(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	registerFakeBrowser(t, db, "reg-b", "other-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "门禁", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	// Kernel change, profile rename/swap and both deletions are
	// refused while running; a name-only edit is fine.
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: "门禁", BrowserID: "reg-b"}); err == nil ||
		!strings.Contains(err.Error(), "正在运行") {
		t.Errorf("kernel change while running error = %v", err)
	}
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{
		ID: inst.ID, Name: "门禁", BrowserID: "reg-a", ProfileAction: ProfileActionRename, ProfileName: "别名",
	}); err == nil || !strings.Contains(err.Error(), "正在运行") {
		t.Errorf("profile rename while running error = %v", err)
	}
	if err := svc.DeleteInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "正在运行") {
		t.Errorf("delete while running error = %v", err)
	}
	if err := svc.DeleteInstanceAndProfile("", inst.ID); err == nil || !strings.Contains(err.Error(), "正在运行") {
		t.Errorf("purge while running error = %v", err)
	}
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: "门禁改名", BrowserID: "reg-a"}); err != nil {
		t.Errorf("name-only edit while running refused: %v", err)
	}
	// After stopping, the kernel change goes through.
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: "门禁改名", BrowserID: "reg-b"})
	if err != nil {
		t.Fatalf("kernel change after stop: %v", err)
	}
	if updated.KernelID != "reg-b" {
		t.Errorf("kernel after change = %q", updated.KernelID)
	}
	// And a stopped instance deletes normally.
	if err := svc.DeleteInstance("", inst.ID); err != nil {
		t.Errorf("delete after stop: %v", err)
	}
}

func TestTwoInstancesRunIndependently(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	a, err := svc.CreateInstance("", CreateInstanceInput{Name: "甲", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateInstance("", CreateInstanceInput{Name: "乙", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", a.ID); err != nil {
		t.Fatal(err)
	}
	procA := fl.lastProc
	if err := svc.StartInstance("", b.ID); err != nil {
		t.Fatal(err)
	}
	procB := fl.lastProc
	// Each launch carried its OWN instance folder.
	if !strings.Contains(strings.Join(fl.lastArgs[procA.pid], " "), "P-1") ||
		!strings.Contains(strings.Join(fl.lastArgs[procB.pid], " "), "P-2") {
		t.Fatalf("launch args crossed: A=%v B=%v", fl.lastArgs[procA.pid], fl.lastArgs[procB.pid])
	}
	// Closing A leaves B running.
	if err := svc.StopInstance("", a.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, a.ID, model.StatusStopped)
	got, _ := svc.GetInstance(b.ID)
	if got.Status != model.StatusRunning {
		t.Fatalf("B status after A stopped = %q, want running", got.Status)
	}
	if err := svc.StopInstance("", b.ID); err != nil {
		t.Fatal(err)
	}
}

func TestReattachAfterRestart(t *testing.T) {
	fl := newFakeLauncher()
	svc1, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc1.CreateInstance("", CreateInstanceInput{Name: "跨重启", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc1.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	// A new service over the same database (DeBrowser restarted)
	// re-identifies the still-running browser and can close it.
	svc2, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	got, err := svc2.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusRunning {
		t.Fatalf("status after restart = %q, want running (re-attached)", got.Status)
	}
	if err := svc2.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop after re-attach: %v", err)
	}
	waitStatus(t, svc2, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the stop after re-attach: %v", err)
	}
}

// After a restart re-attaches a still-running browser, the user
// closing that browser's window is settled by the new session's
// watcher exactly like a locally started one: status stopped,
// runtime record deleted.
func TestReattachThenUserCloseSettles(t *testing.T) {
	fl := newFakeLauncher()
	svc1, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc1.CreateInstance("", CreateInstanceInput{Name: "接管后自关", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc1.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	svc2, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	if got, _ := svc2.GetInstance(inst.ID); got.Status != model.StatusRunning {
		t.Fatalf("status after restart = %q, want running (re-attached)", got.Status)
	}
	fl.lastProc.exit(0)
	waitStatus(t, svc2, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record survived the re-attached browser's exit: %v", err)
	}
}

func seedRuntimeRow(t *testing.T, db *database.DB, inst model.Instance, rt model.InstanceRuntime) {
	t.Helper()
	st := store.New(db)
	if err := st.UpsertInstanceRuntime(t.Context(), rt); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if err := st.SetInstanceStatus(t.Context(), inst.ID, model.StatusRunning); err != nil {
		t.Fatalf("seed status: %v", err)
	}
}

func TestReconcileGoneAndForeignProcesses(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	st := store.New(db)

	// Recorded process is simply gone.
	gone, err := svc.CreateInstance("", CreateInstanceInput{Name: "已消失", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	seedRuntimeRow(t, db, gone, model.InstanceRuntime{
		InstanceID: gone.ID, PID: 7777, ProcessCreatedAt: 42, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: gone.UserDataDir,
	})
	// Recorded PID now belongs to a process using ANOTHER
	// instance folder: provably not ours.
	foreign, err := svc.CreateInstance("", CreateInstanceInput{Name: "被冒名", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(8888, runner.Info{
		PID: 8888, CreatedAt: 4242, ExecutablePath: "fake-browser.exe",
		CommandLine: `fake-browser.exe --user-data-dir=C:\somewhere\else`, HasCommandLine: true,
	})
	seedRuntimeRow(t, db, foreign, model.InstanceRuntime{
		InstanceID: foreign.ID, PID: 8888, ProcessCreatedAt: 4242, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: foreign.UserDataDir,
	})

	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	for _, inst := range []model.Instance{gone, foreign} {
		got, err := fresh.GetInstance(inst.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != model.StatusStopped {
			t.Errorf("%s status = %q, want stopped", inst.Name, got.Status)
		}
		if _, err := st.GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s runtime record survived reconciliation: %v", inst.Name, err)
		}
	}
	_ = context.Background
}

func TestReconcileUnverifiableProcessHoldsUnknown(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "待确认", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	// The process exists and matches PID + creation + executable,
	// but its command line cannot be read: ownership of the
	// instance folder is unproven, so the instance must be held
	// in the unknown state and every risky operation refused.
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 31337, ExecutablePath: "fake-browser.exe", HasCommandLine: false,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 31337, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	got, err := fresh.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	if err := fresh.StartInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "无法确认") {
		t.Errorf("start in unknown state error = %v", err)
	}
	if err := fresh.StopInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "无法确认") {
		t.Errorf("stop in unknown state error = %v", err)
	}
	if err := fresh.DeleteInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "无法确认") {
		t.Errorf("delete in unknown state error = %v", err)
	}
	if _, err := fresh.UpdateInstance("", UpdateInstanceInput{
		ID: inst.ID, Name: "待确认", BrowserID: "reg-a", ProfileAction: ProfileActionRename, ProfileName: "改",
	}); err == nil || !strings.Contains(err.Error(), "无法确认") {
		t.Errorf("profile change in unknown state error = %v", err)
	}
	// The runtime record is KEPT: the process may still be the
	// instance's browser, and dropping the record would lose the
	// only link to it.
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Errorf("runtime record dropped in unknown state: %v", err)
	}
}

func TestRuntimeOperationsAreRecorded(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	logs, err := logging.NewSystem(cfg)
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	svc.Auditor = logs
	inst, err := svc.CreateInstance("op-create-x", CreateInstanceInput{Name: "记录实例", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("op-start-x", inst.ID); err != nil {
		t.Fatal(err)
	}
	// Spontaneous exit produces a runtime record.
	fl.lastProc.exit(0)
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	opLog := readLogsOf(t, cfg.Layout.Logs, "operation")
	for _, want := range []string{
		`"action":"instance.start"`, `"operation_id":"op-start-x"`, `"status":"success"`,
	} {
		if !strings.Contains(opLog, want) {
			t.Errorf("operation log missing %q", want)
		}
	}
	runLog := readLogsOf(t, cfg.Layout.Logs, "run")
	if !strings.Contains(runLog, "浏览器进程已退出") {
		t.Errorf("runtime log missing the process-exit record:\n%s", runLog)
	}
	// The user-chosen instance name is free-form input and must
	// never reach the logs.
	if strings.Contains(opLog+runLog, "记录实例") {
		t.Error("logs contain the instance name (free-form user input)")
	}
}

// --- Strict identity verification (Stage 9B review round) ---

// resolvedProfileDir returns the exact absolute folder the
// service launches the instance with: its stored folder resolved
// the same way the service resolves it at launch.
func resolvedProfileDir(t *testing.T, cfg config.Config, inst model.Instance) string {
	t.Helper()
	abs := filepath.Join(cfg.Layout.Data, filepath.FromSlash(inst.UserDataDir))
	if resolved, err := resolveFinalPath(abs); err == nil && resolved != "" {
		return resolved
	}
	return abs
}

func requireStartRefusedAndStopped(t *testing.T, svc *InstanceService, db *database.DB, inst model.Instance) {
	t.Helper()
	got, err := svc.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusStopped {
		t.Errorf("status = %q, want stopped after refused start", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runtime record exists after refused start: %v", err)
	}
}

// The instance owns P-1; the process reports P-10 — a path that
// has P-1 as a string prefix. Substring matching would accept
// it; exact argument comparison must refuse the launch.
func TestStartRejectsPrefixConfusableProfileDir(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "前缀混淆", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	dir := resolvedProfileDir(t, cfg, inst)
	fl.cmdlineOverride = "fake-browser.exe --user-data-dir=" + filepath.Join(filepath.Dir(dir), "P-10")
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start accepted a process using P-10 for the P-1 instance")
	}
	requireStartRefusedAndStopped(t, svc, db, inst)
}

// A repeated --user-data-dir argument is ambiguous even when
// both copies name the right folder: refuse.
func TestStartRejectsDuplicateUserDataDirArg(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "重复参数", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	dir := resolvedProfileDir(t, cfg, inst)
	fl.cmdlineOverride = "fake-browser.exe --user-data-dir=" + dir + " --user-data-dir=" + dir
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start accepted a duplicated --user-data-dir argument")
	}
	requireStartRefusedAndStopped(t, svc, db, inst)
}

// No --user-data-dir argument at all: the process's folder
// ownership is unproven, refuse.
func TestStartRejectsMissingUserDataDirArg(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "缺少参数", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.cmdlineOverride = "fake-browser.exe --no-first-run"
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start accepted a process without --user-data-dir")
	}
	requireStartRefusedAndStopped(t, svc, db, inst)
}

// The space-separated form is a legitimate launch shape and must
// verify exactly like the equals form.
func TestStartAcceptsSpaceSeparatedUserDataDirArg(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "空格形式", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.cmdlineOverride = "fake-browser.exe --user-data-dir " + resolvedProfileDir(t, cfg, inst)
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start with space-separated argument: %v", err)
	}
	waitStatus(t, svc, inst.ID, model.StatusRunning)
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// An unreadable command line at launch verification is a failed
// verification (fail-closed), not a skipped check: the start is
// refused, nothing is recorded, and the instance can still start
// once the signal is readable.
func TestStartFailsClosedWhenCommandLineUnreadable(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "不可读命令行", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.noCmdline = true
	err = svc.StartInstance("", inst.ID)
	if err == nil || !strings.Contains(err.Error(), "命令行") {
		t.Fatalf("start with unreadable command line error = %v, want a fail-closed refusal", err)
	}
	requireStartRefusedAndStopped(t, svc, db, inst)
	fl.noCmdline = false
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start once the command line is readable: %v", err)
	}
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A launch whose child could not be tracked and could NOT be
// confirmed terminated leaves a possibly-running browser: the
// instance is held at unknown with a creation-time-less runtime
// record, and every risky operation is frozen.
func TestStartUntrackedChildHoldsUnknown(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	registerFakeBrowser(t, db, "reg-b", "fake-browser-2.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "失联进程", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.launchErr = &runner.UntrackedChildError{PID: 4321, ChildExited: false, Err: errors.New("tracking failed")}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("untracked launch reported success")
	}
	got, err := svc.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	rt, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("runtime record: %v", err)
	}
	if rt.PID != 4321 || rt.ProcessCreatedAt != 0 {
		t.Errorf("record = pid %d created %d, want pid 4321 and no creation time", rt.PID, rt.ProcessCreatedAt)
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Error("second start allowed in the unknown hold")
	}
	if err := svc.StopInstance("", inst.ID); err == nil {
		t.Error("stop allowed in the unknown hold")
	}
	if err := svc.DeleteInstance("", inst.ID); err == nil {
		t.Error("delete allowed in the unknown hold")
	}
	if err := svc.DeleteInstanceAndProfile("", inst.ID); err == nil {
		t.Error("delete with profile allowed in the unknown hold")
	}
	// Metadata edits endanger no profile data and stay
	// available in the unknown hold: a kernel change takes
	// effect at the next launch (impossible while held) and a
	// name change touches nothing but the record.
	updated, err := svc.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: inst.Name, BrowserID: "reg-b"})
	if err != nil {
		t.Fatalf("kernel change refused in the unknown hold: %v", err)
	}
	if updated.KernelID != "reg-b" {
		t.Errorf("kernel after change = %q, want reg-b", updated.KernelID)
	}
	if _, err := svc.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: "未知时改名", BrowserID: "reg-b"}); err != nil {
		t.Errorf("name-only edit refused in the unknown hold: %v", err)
	}
}

// When the untracked child WAS confirmed terminated, the launch
// is a plain failure: stopped, no record, and the instance can
// start normally afterwards.
func TestStartUntrackedChildExitedIsPlainFailure(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "已清理", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.launchErr = &runner.UntrackedChildError{PID: 4322, ChildExited: true, Err: errors.New("tracking failed")}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("untracked launch reported success")
	}
	requireStartRefusedAndStopped(t, svc, db, inst)
	fl.launchErr = nil
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start after the plain failure: %v", err)
	}
	if err := svc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A runtime record without a creation time (written by the
// untracked-launch hold) can never fully verify at the next
// startup: even with executable and command line matching, the
// process is NOT adopted and the instance stays unknown.
func TestReconcileRecordWithoutCreationTimeStaysUnknown(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "无创建时间记录", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 4242, ExecutablePath: "fake-browser.exe",
		CommandLine: "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst), HasCommandLine: true,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 0, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	got, err := fresh.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown (a creation-time-less record must never be adopted)", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Errorf("runtime record was dropped instead of held: %v", err)
	}
	if err := fresh.StartInstance("", inst.ID); err == nil {
		t.Error("start allowed for an instance whose record cannot verify")
	}
}

// The same record resolves itself once the process is gone: the
// record is cleared and the instance returns to stopped.
func TestReconcileRecordWithoutCreationTimeGoneClears(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "无创建时间已退出", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9998, ProcessCreatedAt: 0, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	got, err := fresh.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusStopped {
		t.Fatalf("status = %q, want stopped", got.Status)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runtime record survived after the process was gone: %v", err)
	}
}

// --- Round 2: abort cleanup confirmation + mandatory exe path ---

func statusOf(t *testing.T, svc *InstanceService, id string) model.InstanceStatus {
	t.Helper()
	got, err := svc.GetInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

// Verification fails after the child already exited on its own:
// the abort is an ordinary start failure — stopped, no record,
// and the instance can start again right away.
func TestStartAbortWhenChildAlreadyExited(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "验证前已退出", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	code := 3
	fl.exitOnLaunch = &code
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start succeeded for a child that exited during the settle window")
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusStopped {
		t.Fatalf("status = %q, want stopped", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record exists after a confirmed-exit abort: %v", err)
	}
	fl.exitOnLaunch = nil
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("restart after confirmed-exit abort: %v", err)
	}
}

// Verification fails and the graceful close succeeds: the exit
// is confirmed through the close, so this stays an ordinary
// start failure and nothing is held.
func TestStartAbortGracefulCloseConfirmsExit(t *testing.T) {
	fl := newFakeLauncher()
	fl.spoofDir = true
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止关闭成功", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start succeeded with a foreign user-data-dir")
	}
	if fl.lastProc.closes != 1 {
		t.Fatalf("graceful closes = %d, want exactly 1 (the exact child)", fl.lastProc.closes)
	}
	select {
	case <-fl.lastProc.exited:
	default:
		t.Fatal("child did not exit after the graceful close")
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusStopped {
		t.Fatalf("status = %q, want stopped", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record held after a confirmed exit: %v", err)
	}
	fl.spoofDir = false
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("restart after graceful-close abort: %v", err)
	}
}

// The close call returns an error but the process has in fact
// exited (a close error racing a real exit): the observed exit
// wins and the abort is an ordinary start failure.
func TestStartAbortCloseErrorButChildExited(t *testing.T) {
	fl := newFakeLauncher()
	fl.spoofDir = true
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "关闭报错但已退出", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.closeExitsOnLaunch = true
	fl.closeErrOnLaunch = errors.New("close raced with exit")
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start succeeded with a foreign user-data-dir")
	} else if !strings.Contains(err.Error(), "未使用该实例的实例文件") {
		t.Fatalf("error = %v, want the original verification failure (ordinary abort)", err)
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusStopped {
		t.Fatalf("status = %q, want stopped (exit was confirmed despite the close error)", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record held after a confirmed exit: %v", err)
	}
}

// The close fails and the process is still alive (WM_CLOSE
// ineffective, no window, a confirmation dialog — the class of
// failures where the request was sent but nothing exited): the
// abort must NOT be an ordinary failure. The instance is held
// unknown with a COMPLETE runtime record, every risky operation
// is frozen, other instances are untouched, and a later restart
// clears the hold once the process is gone.
func TestStartAbortCloseFailsProcessAliveHoldsUnknown(t *testing.T) {
	fl := newFakeLauncher()
	fl.spoofDir = true
	fl.closeErrOnLaunch = errors.New("no top-level window")
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	registerFakeBrowser(t, db, "reg-b", "fake-browser-b.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止无法确认退出", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateInstance("", CreateInstanceInput{Name: "不受影响的实例", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.StartInstance("", inst.ID)
	if err == nil {
		t.Fatal("start succeeded with a foreign user-data-dir")
	}
	if !strings.Contains(err.Error(), "无法确认") {
		t.Fatalf("error = %v, want it to state the exit could not be confirmed", err)
	}
	child := fl.lastProc
	if got := statusOf(t, svc, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown", got)
	}
	rt, rerr := store.New(db).GetInstanceRuntime(t.Context(), inst.ID)
	if rerr != nil {
		t.Fatalf("runtime record missing for the held instance: %v", rerr)
	}
	if rt.PID != child.pid || rt.ProcessCreatedAt != child.created || rt.ProcessCreatedAt == 0 {
		t.Fatalf("record = pid %d created %d, want the child's pid %d and its real creation time %d",
			rt.PID, rt.ProcessCreatedAt, child.pid, child.created)
	}
	// Every risky operation is frozen while the exit is
	// unconfirmed.
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Error("second start allowed while exit unconfirmed")
	}
	if err := svc.StopInstance("", inst.ID); err == nil {
		t.Error("stop allowed while exit unconfirmed")
	}
	if err := svc.DeleteInstance("", inst.ID); err == nil {
		t.Error("delete allowed while exit unconfirmed")
	}
	if err := svc.DeleteInstanceAndProfile("", inst.ID); err == nil {
		t.Error("delete-with-profile allowed while exit unconfirmed")
	}
	// Metadata edits stay available while the exit is
	// unconfirmed; only profile operations stay frozen.
	upd := UpdateInstanceInput{ID: inst.ID, Name: "中止无法确认退出", BrowserID: "reg-b"}
	if _, err := svc.UpdateInstance("", upd); err != nil {
		t.Errorf("kernel change refused while exit unconfirmed: %v", err)
	}
	upd.ProfileAction = "rename"
	upd.ProfileName = "改名尝试"
	if _, err := svc.UpdateInstance("", upd); err == nil {
		t.Error("profile rename allowed while exit unconfirmed")
	}
	// The cleanup touched ONLY this launch's process object:
	// exactly one close attempt, on the child itself. The other
	// instance starts and stops completely normally.
	if child.closes != 1 {
		t.Fatalf("close attempts on the child = %d, want 1", child.closes)
	}
	fl.spoofDir = false
	fl.closeErrOnLaunch = nil
	if err := svc.StartInstance("", other.ID); err != nil {
		t.Fatalf("other instance start affected by the hold: %v", err)
	}
	if err := svc.StopInstance("", other.ID); err != nil {
		t.Fatalf("other instance stop affected by the hold: %v", err)
	}
	// Once the process is gone, a restart's reconciliation
	// clears the record and the instance is stopped again.
	child.exit(0)
	// The hold kept the process watched in THIS session too:
	// the unified watcher settles it without any restart —
	// record deleted, hold lifted, status stopped.
	waitStatus(t, svc, inst.ID, model.StatusStopped)
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record not cleared by the in-session watcher: %v", err)
	}
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	if got := statusOf(t, fresh, inst.ID); got != model.StatusStopped {
		t.Fatalf("after restart with the process gone: status = %q, want stopped", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record not cleared after the process disappeared: %v", err)
	}
}

// A close that drags past the caller's timeout and still leaves
// the process alive lands in the same unknown hold.
func TestStartAbortSlowCloseHoldsUnknown(t *testing.T) {
	old := abortCloseTimeout
	abortCloseTimeout = 50 * time.Millisecond
	defer func() { abortCloseTimeout = old }()
	fl := newFakeLauncher()
	fl.spoofDir = true
	fl.closeErrOnLaunch = errors.New("close timed out")
	fl.closeDelayOnLaunch = 300 * time.Millisecond
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "中止关闭超时", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err == nil {
		t.Fatal("start succeeded with a foreign user-data-dir")
	}
	if got := statusOf(t, svc, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown after a timed-out close with the process alive", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Fatalf("runtime record missing for the held instance: %v", err)
	}
	fl.lastProc.exit(0)
}

// A runtime record WITHOUT an executable path can never be
// adopted, even when every other signal matches: an empty path
// is not a wildcard.
func TestReconcileRecordWithoutExePathStaysUnknown(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "记录缺可执行路径", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 4242, ExecutablePath: "fake-browser.exe",
		CommandLine: "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst), HasCommandLine: true,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 4242, Platform: runtime.GOOS,
		ExecutablePath: "", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	if got := statusOf(t, fresh, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown (record without an executable path must not be adopted)", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Errorf("runtime record was dropped instead of held: %v", err)
	}
	if err := fresh.StartInstance("", inst.ID); err == nil {
		t.Error("start allowed for an instance whose record cannot verify")
	}
}

// A live process whose executable path cannot be READ can never
// be adopted either.
func TestReconcileUnreadableExePathStaysUnknown(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "进程路径不可读", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 4242, ExecutablePath: "",
		CommandLine: "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst), HasCommandLine: true,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 4242, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	if got := statusOf(t, fresh, inst.ID); got != model.StatusUnknown {
		t.Fatalf("status = %q, want unknown (unreadable executable path must not be adopted)", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); err != nil {
		t.Errorf("runtime record was dropped instead of held: %v", err)
	}
}

// Both paths present but different: the process at the recorded
// PID is provably a different browser — record cleared, instance
// stopped — even though creation time and folder argument match.
func TestReconcileExeMismatchIsNotOurs(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "可执行路径不一致", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 4242, ExecutablePath: "other-browser.exe",
		CommandLine: "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst), HasCommandLine: true,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 4242, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	if got := statusOf(t, fresh, inst.ID); got != model.StatusStopped {
		t.Fatalf("status = %q, want stopped (executable mismatch disproves identity)", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record not cleared for a disproven process: %v", err)
	}
}

// A case-only difference in the executable path is the same
// path under Windows comparison rules: adoption proceeds (and
// the adopted process can be stopped normally).
func TestReconcileExeCaseDifferenceAdopts(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "路径大小写差异", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 4242, ExecutablePath: "FAKE-BROWSER.EXE",
		CommandLine: "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst), HasCommandLine: true,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 4242, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	if got := statusOf(t, fresh, inst.ID); got != model.StatusRunning {
		t.Fatalf("status = %q, want running (case-only path difference must still adopt)", got)
	}
	if err := fresh.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop adopted instance: %v", err)
	}
}

// The executable path matching does not rescue a record whose
// creation time differs: the PID was recycled, the record is
// cleared and the instance is stopped.
func TestReconcileExeMatchCreatedMismatchIsNotOurs(t *testing.T) {
	fl := newFakeLauncher()
	svc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "创建时间不一致", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	fl.plant(9999, runner.Info{
		PID: 9999, CreatedAt: 9999, ExecutablePath: "fake-browser.exe",
		CommandLine: "fake-browser.exe --user-data-dir=" + resolvedProfileDir(t, cfg, inst), HasCommandLine: true,
	})
	seedRuntimeRow(t, db, inst, model.InstanceRuntime{
		InstanceID: inst.ID, PID: 9999, ProcessCreatedAt: 4242, Platform: runtime.GOOS,
		ExecutablePath: "fake-browser.exe", UserDataDir: inst.UserDataDir,
	})
	fresh, err := newInstanceService(db, cfg.Layout, slog.Default(), fl)
	if err != nil {
		t.Fatalf("fresh service: %v", err)
	}
	if got := statusOf(t, fresh, inst.ID); got != model.StatusStopped {
		t.Fatalf("status = %q, want stopped (creation-time mismatch disproves identity)", got)
	}
	if _, err := store.New(db).GetInstanceRuntime(t.Context(), inst.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("runtime record not cleared for a recycled PID: %v", err)
	}
}
