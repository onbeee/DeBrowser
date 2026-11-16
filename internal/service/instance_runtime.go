package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/runner"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Stage 9B runtime management. The rules that keep this safe:
//
//   - DeBrowser manages only the browser processes it launched
//     itself, one per instance, each tracked by a process handle
//     plus a durable identity record (instance_runtime): PID +
//     process creation time + executable path + the exact
//     instance folder in the command line.
//   - A PID alone is never trusted: the OS recycles PIDs, so
//     every re-identification verifies creation time, executable
//     path, and — when the OS lets us read it — the command
//     line's --user-data-dir argument.
//   - A process whose identity cannot be CONFIRMED is never
//     closed, never adopted, and its instance is held in the
//     "unknown" state, which blocks every operation that could
//     endanger its data.
//   - Closing is always the graceful request (the equivalent of
//     the user closing the window). Nothing is force-killed, and
//     a close that does not complete is reported as such — the
//     instance stays "running".
//   - DeBrowser exiting never closes browsers: launched processes
//     are independent OS processes, and their runtime records let
//     the next DeBrowser start re-identify the ones still alive.

// startSettleDelay is how long a freshly launched process must
// survive before the launch counts as successful (a field so
// tests can shorten it).
var startSettleDelay = 300 * time.Millisecond

// closeTimeout bounds one graceful close request.
var closeTimeout = 20 * time.Second

// reconcileEvent is one startup-reconciliation outcome, buffered
// until the unified record is wired (see ReportReconciliation).
type reconcileEvent struct {
	instanceID string
	action     string
	status     string
	detail     string
}

func (s *InstanceService) noteReconcile(instanceID, action, status, detail string) {
	s.reconcileEvents = append(s.reconcileEvents, reconcileEvent{
		instanceID: instanceID, action: action, status: status, detail: detail,
	})
}

// ReportReconciliation flushes the startup runtime reconciliation
// outcomes into the unified record. main calls it right after
// wiring the Auditor (the reconciliation itself runs in the
// constructor, before an Auditor can exist).
func (s *InstanceService) ReportReconciliation() {
	if s.Auditor == nil {
		return
	}
	for _, ev := range s.reconcileEvents {
		meta := map[string]string{"instance_id": ev.instanceID}
		if ev.detail != "" {
			meta["detail"] = ev.detail
		}
		s.Auditor.RecordRuntime("浏览器实例", ev.action, ev.status, meta)
	}
	s.reconcileEvents = nil
}

// reconcileRuntimeAtStartup brings persisted state in line with
// live process facts, once, at service construction:
//
//   - instances persisted "running" with NO runtime record are
//     pre-tracking leftovers: stopped (store.ReconcileInstance
//     Statuses);
//   - each runtime record is verified against the live process
//     (see verifyRecordedProcess): verified processes are
//     re-attached and watched; disproved records are removed and
//     their instances stopped; processes that exist but cannot be
//     confirmed are left recorded and their instances set to
//     "unknown" — a safe hold, neither closed nor acted on.
func (s *InstanceService) reconcileRuntimeAtStartup(c context.Context) error {
	if n, err := s.store.ReconcileInstanceStatuses(c); err != nil {
		return err
	} else if n > 0 {
		s.log.Info("reconciled stale running instances to stopped", "count", n)
	}
	records, err := s.store.ListInstanceRuntimes(c)
	if err != nil {
		return err
	}
	for _, rt := range records {
		s.reconcileOne(c, rt)
	}
	return nil
}

// verifyOutcome classifies a recorded process against the live OS.
type verifyOutcome int

const (
	verifyGone      verifyOutcome = iota // provably not running anymore
	verifyNotOurs                        // a live process exists but is provably NOT this instance's browser
	verifyConfirmed                      // identity fully verified
	verifyUnknown                        // exists (or may exist) but cannot be confirmed either way
)

// verifyRecordedProcess compares one runtime record with the live
// process at its PID.
func (s *InstanceService) verifyRecordedProcess(rt model.InstanceRuntime) (verifyOutcome, runner.Process) {
	if rt.Platform != "" && rt.Platform != runtime.GOOS {
		return verifyUnknown, nil // recorded on another OS: unverifiable here
	}
	info, err := s.launcher.Inspect(rt.PID)
	if err != nil {
		if errors.Is(err, runner.ErrProcessGone) {
			return verifyGone, nil
		}
		// Access denied (or any other inspection failure): the
		// process may well be alive — nothing may be concluded.
		return verifyUnknown, nil
	}
	if info.CreatedAt != rt.ProcessCreatedAt {
		return verifyNotOurs, nil // PID recycled by another process
	}
	if rt.ExecutablePath != "" && info.ExecutablePath != "" &&
		!strings.EqualFold(filepath.Clean(info.ExecutablePath), filepath.Clean(rt.ExecutablePath)) {
		return verifyNotOurs, nil
	}
	absDir := filepath.Join(s.layout.Data, filepath.FromSlash(rt.UserDataDir))
	if info.HasCommandLine {
		// The launch used the RESOLVED folder path; accept the
		// command line naming either the lexical or the resolved
		// form of the instance folder.
		dirs := []string{absDir}
		if resolved, rerr := resolveFinalPath(absDir); rerr == nil && resolved != absDir {
			dirs = append(dirs, resolved)
		}
		uses := false
		for _, d := range dirs {
			if commandLineUsesDir(info.CommandLine, d) {
				uses = true
				break
			}
		}
		if !uses {
			return verifyNotOurs, nil
		}
		proc, err := s.launcher.Open(rt.PID, rt.ProcessCreatedAt)
		if err != nil {
			return verifyUnknown, nil
		}
		return verifyConfirmed, proc
	}
	// The command line could not be read: PID + creation time +
	// executable agree, but the instance-folder ownership — the
	// fact that decides whether closing this process is safe —
	// is unproven. Hold at unknown.
	return verifyUnknown, nil
}

// commandLineUsesDir reports whether a command line carries a
// --user-data-dir argument naming exactly absDir. The check is a
// substring test on the raw command line: argument values are
// never re-split (paths may contain spaces), and absDir itself is
// the exact absolute path that was passed at launch.
func commandLineUsesDir(commandLine, absDir string) bool {
	return strings.Contains(commandLine, "--user-data-dir") && strings.Contains(commandLine, absDir)
}

func (s *InstanceService) reconcileOne(c context.Context, rt model.InstanceRuntime) {
	inst, err := s.store.GetInstance(c, rt.InstanceID)
	if err != nil {
		// The instance is gone (its record should have cascaded);
		// drop the orphan record.
		_ = s.store.DeleteInstanceRuntime(c, rt.InstanceID)
		return
	}
	outcome, proc := s.verifyRecordedProcess(rt)
	switch outcome {
	case verifyConfirmed:
		s.rtMu.Lock()
		s.procs[inst.ID] = proc
		s.rtMu.Unlock()
		_ = s.store.SetInstanceStatus(c, inst.ID, model.StatusRunning)
		go s.watchProcess(inst.ID, proc)
		s.noteReconcile(inst.ID, "启动时重新识别运行中的实例", logging.StatusSuccess, "")
		s.log.Info("re-attached to running browser instance", "id", inst.ID, "pid", rt.PID)
	case verifyGone, verifyNotOurs:
		_ = s.store.DeleteInstanceRuntime(c, rt.InstanceID)
		_ = s.store.SetInstanceStatus(c, inst.ID, model.StatusStopped)
		action := "启动时确认实例已关闭"
		if outcome == verifyNotOurs {
			action = "启动时确认原记录进程已不属于该实例"
		}
		s.noteReconcile(inst.ID, action, logging.StatusSuccess, "")
		s.log.Info("instance runtime record cleared at startup", "id", inst.ID, "outcome", int(outcome))
	case verifyUnknown:
		// Safe hold: the runtime record and the persisted
		// "running" status are KEPT (the process may still be
		// this instance's browser), and the instance's effective
		// status becomes "unknown" for this run — every risky
		// operation refuses on it, and the next startup
		// reconciles the record again.
		s.rtMu.Lock()
		s.unverified[inst.ID] = true
		s.rtMu.Unlock()
		s.noteReconcile(inst.ID, "启动时无法确认实例运行状态", logging.StatusFailed, "进程身份无法确认，已进入安全保持状态")
		s.log.Warn("instance process identity unconfirmed at startup", "id", inst.ID, "pid", rt.PID)
	}
}

// effectiveStatus overlays the in-memory runtime truth (tracked
// processes, in-flight transitions, unverified startup records)
// onto the persisted status.
func (s *InstanceService) effectiveStatus(inst model.Instance) model.InstanceStatus {
	s.rtMu.Lock()
	defer s.rtMu.Unlock()
	if t, ok := s.transient[inst.ID]; ok {
		return t
	}
	if _, ok := s.procs[inst.ID]; ok {
		return model.StatusRunning
	}
	if s.unverified[inst.ID] {
		return model.StatusUnknown
	}
	return inst.Status
}

// applyEffectiveStatuses rewrites the Status of each listed
// instance to its effective status.
func (s *InstanceService) applyEffectiveStatuses(list []model.Instance) {
	for i := range list {
		list[i].Status = s.effectiveStatus(list[i])
	}
}

// watchProcess observes one tracked browser process. When it exits
// on its own — the user closed the window, or the browser crashed
// — the instance's records are brought in line: runtime record
// removed, persisted status updated (stopped for a clean exit,
// error for an abnormal one), and the fact recorded. When a stop
// operation is handling the exit itself it first unregisters the
// process, and this watcher then stands down.
func (s *InstanceService) watchProcess(instanceID string, proc runner.Process) {
	<-proc.Exited()
	s.rtMu.Lock()
	current, tracked := s.procs[instanceID]
	if !tracked || current != proc {
		s.rtMu.Unlock()
		return // a stop operation owns this exit
	}
	delete(s.procs, instanceID)
	s.rtMu.Unlock()

	c, cancel := ctx()
	defer cancel()
	_ = s.store.DeleteInstanceRuntime(c, instanceID)
	code := proc.ExitCode()
	status := model.StatusStopped
	action := "浏览器进程已退出"
	recStatus := logging.StatusSuccess
	if code != 0 {
		status = model.StatusError
		action = "浏览器进程异常退出"
		recStatus = logging.StatusFailed
	}
	_ = s.store.SetInstanceStatus(c, instanceID, status)
	if s.Auditor != nil {
		s.Auditor.RecordRuntime("浏览器实例", action, recStatus, map[string]string{
			"instance_id": instanceID,
			"pid":         strconv.Itoa(proc.PID()),
			"exit_code":   strconv.Itoa(code),
		})
	}
	s.log.Info("browser process exited", "instance", instanceID, "pid", proc.PID(), "exitCode", code)
}

// StartInstance launches the instance's browser (recorded).
func (s *InstanceService) StartInstance(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "浏览器实例", "instance.start", "instance", id)
	err := s.startInstance(id, op)
	finishOp(op, err)
	return err
}

func (s *InstanceService) startInstance(id string, op *logging.Operation) error {
	c, cancel := ctx()
	defer cancel()
	inst, err := s.store.GetInstance(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	s.rtMu.Lock()
	if _, running := s.procs[id]; running {
		s.rtMu.Unlock()
		return fmt.Errorf("实例已在运行中")
	}
	if t, busy := s.transient[id]; busy {
		s.rtMu.Unlock()
		if t == model.StatusStarting {
			return fmt.Errorf("实例正在启动中")
		}
		return fmt.Errorf("实例正在关闭中")
	}
	if s.unverified[id] {
		s.rtMu.Unlock()
		return fmt.Errorf("实例运行状态无法确认，不能启动")
	}
	s.transient[id] = model.StatusStarting
	s.rtMu.Unlock()
	fail := func(err error) error {
		s.rtMu.Lock()
		delete(s.transient, id)
		s.rtMu.Unlock()
		return err
	}

	// Kernel: the association must exist and its registration
	// must still be there. The executable path is the
	// registration's CURRENT path.
	if inst.KernelID == "" {
		return fail(fmt.Errorf("实例尚未关联浏览器内核，无法启动"))
	}
	reg, err := s.store.GetExternalBrowser(c, inst.KernelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fail(fmt.Errorf("实例关联的浏览器内核登记不存在，无法启动"))
		}
		return fail(err)
	}
	exe := reg.ExecutablePath
	op.Step("解析浏览器内核", logging.StatusSuccess, map[string]string{"kernel_id": reg.ID})

	// Instance file: must be a managed folder directly under
	// data/profiles. A missing folder at first launch is created
	// empty; anything abnormal refuses the launch — never a
	// silent switch to another folder.
	folder := profileFolder(inst.UserDataDir)
	if folder == "" {
		return fail(fmt.Errorf("实例文件路径异常，无法启动"))
	}
	absDir := filepath.Join(s.layout.Profiles, folder)
	launchDir := absDir
	if _, lerr := os.Lstat(absDir); lerr == nil {
		resolved, verr := s.verifiedProfileTarget(inst.UserDataDir)
		if verr != nil {
			return fail(fmt.Errorf("实例文件无法安全访问：%w", verr))
		}
		if resolved != "" {
			launchDir = resolved
		}
	} else if errors.Is(lerr, os.ErrNotExist) {
		if err := os.MkdirAll(absDir, 0o755); err != nil {
			return fail(fmt.Errorf("创建实例文件目录失败：%w", err))
		}
		op.Step("创建实例文件目录", logging.StatusSuccess, map[string]string{"profile": folder})
	} else {
		return fail(fmt.Errorf("实例文件无法访问：%w", lerr))
	}
	// No other tracked instance may be using this folder (the
	// unique binding already guarantees this; this is the live
	// double-check).
	s.rtMu.Lock()
	for otherID := range s.procs {
		if otherID == id {
			continue
		}
		if other, oerr := s.store.GetInstance(c, otherID); oerr == nil && other.UserDataDir == inst.UserDataDir {
			s.rtMu.Unlock()
			return fail(fmt.Errorf("实例文件正被其他实例使用，无法启动"))
		}
	}
	s.rtMu.Unlock()

	args := []string{"--user-data-dir=" + launchDir}
	if strings.TrimSpace(inst.StartURL) != "" {
		args = append(args, strings.TrimSpace(inst.StartURL))
	}
	proc, err := s.launcher.Launch(exe, args...)
	if err != nil {
		op.Step("启动浏览器进程", logging.StatusFailed, nil)
		return fail(fmt.Errorf("浏览器启动失败：%w", err))
	}
	op.Step("启动浏览器进程", logging.StatusSuccess, map[string]string{"pid": strconv.Itoa(proc.PID())})

	// A process object is not proof of a running browser: the
	// process must survive the settle window, and its verified
	// identity must match what was launched.
	abort := func(err error) error {
		// This process was started by THIS operation moments ago
		// and never became a tracked instance browser; asking it
		// to close is safe and leaves no phantom behind.
		_ = proc.CloseGracefully(3 * time.Second)
		op.Step("确认浏览器运行", logging.StatusFailed, nil)
		return fail(err)
	}
	select {
	case <-proc.Exited():
		return abort(fmt.Errorf("浏览器启动后立即退出（退出代码 %d）", proc.ExitCode()))
	case <-time.After(startSettleDelay):
	}
	info, ierr := s.launcher.Inspect(proc.PID())
	if ierr != nil {
		if errors.Is(ierr, runner.ErrProcessGone) {
			return abort(fmt.Errorf("浏览器启动后立即退出"))
		}
		return abort(fmt.Errorf("无法确认浏览器进程状态：%w", ierr))
	}
	if info.ExecutablePath != "" && !strings.EqualFold(filepath.Clean(info.ExecutablePath), filepath.Clean(exe)) {
		return abort(fmt.Errorf("启动的进程与所选浏览器内核不一致"))
	}
	if info.HasCommandLine && !commandLineUsesDir(info.CommandLine, launchDir) {
		return abort(fmt.Errorf("浏览器进程未使用该实例的实例文件，已拒绝认定启动成功"))
	}
	op.Step("确认浏览器运行", logging.StatusSuccess, nil)

	rt := model.InstanceRuntime{
		InstanceID:       id,
		PID:              proc.PID(),
		ProcessCreatedAt: proc.CreatedAt(),
		Platform:         runtime.GOOS,
		ExecutablePath:   exe,
		UserDataDir:      inst.UserDataDir,
		StartedAt:        time.Now().Format(time.RFC3339),
	}
	if err := s.store.UpsertInstanceRuntime(c, rt); err != nil {
		return abort(fmt.Errorf("保存运行状态失败：%w", err))
	}
	if err := s.store.SetInstanceStatus(c, id, model.StatusRunning); err != nil {
		_ = s.store.DeleteInstanceRuntime(c, id)
		return abort(fmt.Errorf("保存运行状态失败：%w", err))
	}
	s.rtMu.Lock()
	s.procs[id] = proc
	delete(s.transient, id)
	s.rtMu.Unlock()
	go s.watchProcess(id, proc)
	s.log.Info("browser instance started", "id", id, "pid", proc.PID(), "dir", inst.UserDataDir)
	return nil
}

// StopInstance gracefully closes the instance's browser
// (recorded). Only the tracked, identity-verified process of
// this instance is ever asked to close.
func (s *InstanceService) StopInstance(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "浏览器实例", "instance.stop", "instance", id)
	err := s.stopInstance(id, op)
	finishOp(op, err)
	return err
}

func (s *InstanceService) stopInstance(id string, op *logging.Operation) error {
	c, cancel := ctx()
	defer cancel()
	inst, err := s.store.GetInstance(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	s.rtMu.Lock()
	proc, tracked := s.procs[id]
	if tracked {
		// Unregister first: the exit watcher stands down and this
		// operation owns the outcome.
		delete(s.procs, id)
		s.transient[id] = model.StatusStopping
	}
	s.rtMu.Unlock()
	if !tracked {
		if s.effectiveStatus(inst) == model.StatusUnknown {
			return fmt.Errorf("实例运行状态无法确认，不能执行关闭")
		}
		return fmt.Errorf("实例未在运行")
	}
	clearTransient := func() {
		s.rtMu.Lock()
		delete(s.transient, id)
		s.rtMu.Unlock()
	}
	if err := proc.CloseGracefully(closeTimeout); err != nil {
		// The browser did not close: restore tracking (the exit
		// watcher resumes ownership) and tell the truth — the
		// instance is still running.
		s.rtMu.Lock()
		s.procs[id] = proc
		delete(s.transient, id)
		s.rtMu.Unlock()
		op.Step("关闭浏览器", logging.StatusFailed, nil)
		return fmt.Errorf("关闭未完成：浏览器未能正常退出（%v），实例仍在运行", err)
	}
	clearTransient()
	_ = s.store.DeleteInstanceRuntime(c, id)
	if err := s.store.SetInstanceStatus(c, id, model.StatusStopped); err != nil {
		return err
	}
	op.Step("关闭浏览器", logging.StatusSuccess, nil)
	s.log.Info("browser instance stopped", "id", id, "pid", proc.PID())
	return nil
}
