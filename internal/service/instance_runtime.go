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
//     path, and the command line's --user-data-dir argument —
//     parsed exactly, never substring-matched. Any signal that
//     cannot be read makes the identity unconfirmed, never
//     "close enough".
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

// abortCloseTimeout bounds the graceful close asked of a
// just-launched process whose verification failed. A variable
// so tests can shorten it.
var abortCloseTimeout = 3 * time.Second

// processExited reports whether the tracked process's exit has
// already been observed on its own exit channel — the only
// proof of exit the abort path accepts.
func processExited(p runner.Process) bool {
	select {
	case <-p.Exited():
		return true
	default:
		return false
	}
}

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
//   - instances persisted "running" with NO runtime record and
//     NO protection journal entry are pre-tracking leftovers:
//     stopped (store.ReconcileInstanceStatuses);
//   - each runtime record is verified against the live process
//     (see verifyRecordedProcess): verified processes are
//     re-attached and watched; disproved records are removed and
//     their instances stopped; processes that exist but cannot be
//     confirmed are left recorded and their instances set to
//     "unknown" — a safe hold, neither closed nor acted on;
//   - each protection journal entry (written when a runtime
//     record could not be) is verified with the same standard:
//     confirmed processes are promoted to real runtime records
//     and re-attached; disproved or gone processes clear the
//     journal; anything unverifiable keeps the instance frozen.
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
	protections, err := s.store.ListInstanceProtections(c)
	if err != nil {
		return err
	}
	for _, prot := range protections {
		s.reconcileProtection(c, prot)
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
// process at its PID. Confirmation requires EVERY identity signal
// to check out: creation time, executable path, and the parsed
// --user-data-dir argument naming exactly this instance's folder.
// A signal that DISPROVES the identity (a different process now
// owns the PID) clears the record; a signal that merely cannot
// be READ leaves the instance at "unknown" — never adopted,
// never acted on.
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
	// unprovable collects the signals that could not be checked;
	// any one of them caps the outcome at "unknown".
	unprovable := false
	if rt.ProcessCreatedAt <= 0 {
		// The record was written when tracking failed at launch:
		// no creation time was ever captured, so PID reuse can
		// never be ruled out and the process can never be fully
		// confirmed — at best it stays unknown while it lives.
		unprovable = true
	} else if info.CreatedAt != rt.ProcessCreatedAt {
		return verifyNotOurs, nil // PID recycled by another process
	}
	// The executable path is a mandatory identity signal, not an
	// optional one: a record that lacks it, or a process whose
	// path cannot be read, can never be adopted — an empty path
	// is never treated as a wildcard or skipped over.
	if rt.ExecutablePath == "" || info.ExecutablePath == "" {
		return verifyUnknown, nil
	}
	if !strings.EqualFold(filepath.Clean(info.ExecutablePath), filepath.Clean(rt.ExecutablePath)) {
		return verifyNotOurs, nil // a different browser binary owns this PID
	}
	if !info.HasCommandLine {
		// The instance-folder ownership — the fact that decides
		// whether closing this process is safe — is unproven.
		return verifyUnknown, nil
	}
	absDir := filepath.Join(s.layout.Data, filepath.FromSlash(rt.UserDataDir))
	// The launch used the RESOLVED folder path; accept the
	// argument naming either the lexical or the resolved form of
	// the instance folder — each compared exactly.
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
	if unprovable {
		return verifyUnknown, nil
	}
	proc, err := s.launcher.Open(rt.PID, rt.ProcessCreatedAt)
	if err != nil {
		return verifyUnknown, nil
	}
	return verifyConfirmed, proc
}

// commandLineUsesDir reports whether the command line's
// --user-data-dir argument names exactly absDir. The argument is
// extracted by parsing (runner.UserDataDirArg) and the paths are
// compared whole, after cleaning, case-insensitively per Windows
// path semantics — a folder that merely shares a prefix (P-10
// vs P-1) or a path appearing in any other argument never
// matches.
func commandLineUsesDir(commandLine, absDir string) bool {
	value, ok := runner.UserDataDirArg(commandLine)
	if !ok {
		return false
	}
	return strings.EqualFold(filepath.Clean(value), filepath.Clean(absDir))
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
		// A fallback journal entry for the same instance is
		// superseded by the record's adoption.
		_ = s.store.DeleteInstanceProtection(c, inst.ID)
		go s.watchProcess(inst.ID, proc)
		s.noteReconcile(inst.ID, "启动时重新识别运行中的实例", logging.StatusSuccess, "")
		s.log.Info("re-attached to running browser instance", "id", inst.ID, "pid", rt.PID)
	case verifyGone, verifyNotOurs:
		_ = s.store.DeleteInstanceRuntime(c, rt.InstanceID)
		_ = s.store.DeleteInstanceProtection(c, rt.InstanceID)
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

// reconcileProtection re-judges one fallback protection journal
// entry at startup, with the same strict verification as a
// runtime record. A confirmed process is promoted: the real
// runtime record is written (only then is the journal dropped
// and the process re-attached and watched). A process proven
// gone or foreign clears the journal and stops the instance.
// Anything unverifiable — including a confirmed process whose
// record STILL cannot be written — keeps the journal and the
// frozen "unknown" hold. Definitive outcomes (confirmed, gone,
// not-ours) also clear any unverified mark an earlier check in
// the same startup pass left behind (the runtime record of the
// same instance may have judged a stale process unknown); only
// the unverifiable outcomes keep the mark.
func (s *InstanceService) reconcileProtection(c context.Context, prot model.InstanceProtection) {
	inst, err := s.store.GetInstance(c, prot.InstanceID)
	if err != nil {
		// The instance is gone (its journal entry should have
		// cascaded); drop the orphan entry.
		_ = s.store.DeleteInstanceProtection(c, prot.InstanceID)
		return
	}
	rt := model.InstanceRuntime{
		InstanceID:       prot.InstanceID,
		PID:              prot.PID,
		ProcessCreatedAt: prot.ProcessCreatedAt,
		Platform:         prot.Platform,
		ExecutablePath:   prot.ExecutablePath,
		UserDataDir:      prot.UserDataDir,
		StartedAt:        prot.MarkedAt,
	}
	outcome, proc := s.verifyRecordedProcess(rt)
	switch outcome {
	case verifyConfirmed:
		s.rtMu.Lock()
		_, tracked := s.procs[inst.ID]
		s.rtMu.Unlock()
		if tracked {
			// A runtime record for this instance already
			// re-adopted the process in this same pass; the
			// journal entry is superseded.
			_ = s.store.DeleteInstanceProtection(c, inst.ID)
			s.clearUnverifiedMark(inst.ID)
			return
		}
		if err := s.store.UpsertInstanceRuntime(c, rt); err != nil {
			s.rtMu.Lock()
			s.unverified[inst.ID] = true
			s.rtMu.Unlock()
			s.noteReconcile(inst.ID, "启动时无法确认实例运行状态", logging.StatusFailed, "进程身份已确认，但运行记录仍无法写入，继续保持保护状态")
			s.log.Warn("protection promotion failed; instance stays held", "id", inst.ID, "pid", prot.PID, "err", err)
			return
		}
		_ = s.store.DeleteInstanceProtection(c, inst.ID)
		s.rtMu.Lock()
		s.procs[inst.ID] = proc
		s.rtMu.Unlock()
		_ = s.store.SetInstanceStatus(c, inst.ID, model.StatusRunning)
		// The identity is fully confirmed: any unverified mark
		// left by this pass's runtime-record check (which may
		// have judged a different, stale record unknown) is
		// stale and must not keep the instance at "unknown".
		s.clearUnverifiedMark(inst.ID)
		go s.watchProcess(inst.ID, proc)
		s.noteReconcile(inst.ID, "启动时重新识别运行中的实例", logging.StatusSuccess, "")
		s.log.Info("re-attached to running browser instance (via protection journal)", "id", inst.ID, "pid", prot.PID)
	case verifyGone, verifyNotOurs:
		_ = s.store.DeleteInstanceProtection(c, inst.ID)
		_ = s.store.SetInstanceStatus(c, inst.ID, model.StatusStopped)
		// The process is definitively resolved: clear any
		// unverified mark left by this pass's runtime-record
		// check, or the instance would stay frozen at
		// "unknown" for the whole session although its
		// browser is proven gone / foreign.
		s.clearUnverifiedMark(inst.ID)
		action := "启动时确认实例已关闭"
		if outcome == verifyNotOurs {
			action = "启动时确认原记录进程已不属于该实例"
		}
		s.noteReconcile(inst.ID, action, logging.StatusSuccess, "")
		s.log.Info("instance protection journal cleared at startup", "id", inst.ID, "outcome", int(outcome))
	case verifyUnknown:
		s.rtMu.Lock()
		s.unverified[inst.ID] = true
		s.rtMu.Unlock()
		s.noteReconcile(inst.ID, "启动时无法确认实例运行状态", logging.StatusFailed, "进程身份无法确认，已进入安全保持状态")
		s.log.Warn("instance process identity unconfirmed at startup (protection journal)", "id", inst.ID, "pid", prot.PID)
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

// holdUnconfirmedInstance puts an instance whose browser may
// still be running — without DeBrowser being able to confirm
// its exit or keep watching it — into the "unknown" hold, and
// reports whether the hold was DURABLY established. The
// in-memory unverified mark is set first, so this session
// freezes every risky operation no matter what happens with
// persistence. Durability comes from the database: primarily
// the runtime record itself (with the persisted status set to
// running, exactly like a verified launch), so the next
// startup reconciliation re-judges the process instead of
// assuming it stopped. If — and only if — that record cannot
// be written, a fallback protection journal entry carrying the
// same identity is written instead; startup reconciliation
// re-verifies journal entries with the same strict standard,
// promotes confirmed ones to real runtime records, clears
// proven-gone ones, and keeps the rest frozen. createdAt is
// the process creation time when it was captured (a launch
// whose post-launch verification failed: the identity is
// complete and reconciliation may fully verify and re-adopt
// the process) or 0 when tracking itself failed at launch (no
// creation time was ever captured, so the process can never be
// fully confirmed and is never adopted). A non-nil return
// means NO durable protection exists — only this session's
// memory mark — and the caller must treat the situation as a
// severe error, never as an ordinary start failure.
func (s *InstanceService) holdUnconfirmedInstance(c context.Context, inst model.Instance, exe string, pid int, createdAt int64) error {
	s.rtMu.Lock()
	s.unverified[inst.ID] = true
	s.rtMu.Unlock()
	if pid <= 0 {
		return fmt.Errorf("没有可保护的进程标识")
	}
	now := time.Now().Format(time.RFC3339)
	rt := model.InstanceRuntime{
		InstanceID:       inst.ID,
		PID:              pid,
		ProcessCreatedAt: createdAt,
		Platform:         runtime.GOOS,
		ExecutablePath:   exe,
		UserDataDir:      inst.UserDataDir,
		StartedAt:        now,
	}
	if err := s.store.UpsertInstanceRuntime(c, rt); err != nil {
		prot := model.InstanceProtection{
			InstanceID:       inst.ID,
			PID:              pid,
			ProcessCreatedAt: createdAt,
			Platform:         runtime.GOOS,
			ExecutablePath:   exe,
			UserDataDir:      inst.UserDataDir,
			MarkedAt:         now,
		}
		if perr := s.store.UpsertInstanceProtection(c, prot); perr != nil {
			return fmt.Errorf("运行记录与保护记录均写入失败（运行记录：%v；保护记录：%w）", err, perr)
		}
		s.log.Warn("runtime record write failed; instance held via protection journal", "id", inst.ID, "pid", pid, "err", err)
		return nil
	}
	if err := s.store.SetInstanceStatus(c, inst.ID, model.StatusRunning); err != nil {
		// The record exists, and startup reconciliation derives
		// the hold from the record itself; the persisted status
		// is corrected there. Not a durability failure.
		s.log.Error("hold unconfirmed instance as running failed", "id", inst.ID, "err", err)
	}
	s.log.Warn("instance browser exit unconfirmed; held as unknown", "id", inst.ID, "pid", pid)
	return nil
}

// clearUnverifiedMark lifts the in-memory unknown hold (used
// when an exit that looked unconfirmable is confirmed after
// all, making the hold moot).
func (s *InstanceService) clearUnverifiedMark(id string) {
	s.rtMu.Lock()
	delete(s.unverified, id)
	s.rtMu.Unlock()
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
		var untracked *runner.UntrackedChildError
		if errors.As(err, &untracked) && !untracked.ChildExited {
			// The browser may genuinely be running with this
			// instance's folder while DeBrowser holds no handle
			// on it. Hold the instance at "unknown" so no risky
			// operation — including a second start — can touch
			// it until a later reconciliation resolves the
			// record.
			if herr := s.holdUnconfirmedInstance(c, inst, exe, untracked.PID, 0); herr != nil {
				op.Step("写入无法确认保护记录", logging.StatusFailed, nil)
				return fail(fmt.Errorf("严重错误：浏览器进程可能已启动但无法建立进程跟踪，且保护记录写入失败，重启后可能无法恢复该实例的保护状态：%w", herr))
			}
			return fail(fmt.Errorf("浏览器进程可能已启动，但无法建立进程跟踪，实例已进入无法确认状态：%w", err))
		}
		return fail(fmt.Errorf("浏览器启动失败：%w", err))
	}
	op.Step("启动浏览器进程", logging.StatusSuccess, map[string]string{"pid": strconv.Itoa(proc.PID())})

	// A process object is not proof of a running browser: the
	// process must survive the settle window, and its verified
	// identity must match what was launched.
	abort := func(err error) error {
		op.Step("确认浏览器运行", logging.StatusFailed, nil)
		// This process was started by THIS operation moments ago
		// and never became a tracked instance browser, so asking
		// IT — this exact process object, never a name or PID
		// lookup — to close is safe. But the abort counts as an
		// ordinary start failure only once the process's exit is
		// CONFIRMED: a close request having been sent (WM_CLOSE
		// posted, signal delivered) is not proof of exit, and a
		// close error racing a real exit is still an exit.
		if processExited(proc) {
			op.Step("清理未核验的浏览器进程", logging.StatusSuccess, nil)
			return fail(err)
		}
		if cerr := proc.CloseGracefully(abortCloseTimeout); cerr == nil || processExited(proc) {
			op.Step("清理未核验的浏览器进程", logging.StatusSuccess, nil)
			return fail(err)
		}
		// The process may still be running with this instance's
		// folder and its exit cannot be confirmed. Hold the
		// instance at "unknown" with a complete runtime record
		// (this launch DID capture the identity) so nothing
		// treats it as stopped, risky operations stay frozen,
		// and the next startup reconciliation re-verifies the
		// record — re-adopting the browser if it is still this
		// instance's, clearing it once the process is gone.
		op.Step("清理未核验的浏览器进程", logging.StatusFailed, nil)
		if herr := s.holdUnconfirmedInstance(c, inst, exe, proc.PID(), proc.CreatedAt()); herr != nil {
			// The hold itself could not be persisted. One last
			// resort remains: this operation still holds the
			// exact process object — try once more to close it
			// and confirm the exit. If the exit is confirmed
			// after all, the ordinary failure becomes truthful
			// again and the in-memory mark comes off.
			op.Step("写入无法确认保护记录", logging.StatusFailed, nil)
			if processExited(proc) {
				s.clearUnverifiedMark(inst.ID)
				op.Step("清理未核验的浏览器进程", logging.StatusSuccess, nil)
				return fail(err)
			}
			if cerr := proc.CloseGracefully(abortCloseTimeout); cerr == nil || processExited(proc) {
				s.clearUnverifiedMark(inst.ID)
				op.Step("清理未核验的浏览器进程", logging.StatusSuccess, nil)
				return fail(err)
			}
			return fail(fmt.Errorf("严重错误：无法确认浏览器进程是否已退出，且保护记录写入失败，重启后可能无法恢复该实例的保护状态：%w", herr))
		}
		return fail(fmt.Errorf("无法确认浏览器进程是否已退出，实例已进入无法确认状态：%w", err))
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
	// Identity verification is fail-closed: a signal that cannot
	// be read at all is a failed verification, never a skipped
	// one. The launch counts as successful only when the
	// executable matches the chosen kernel AND the parsed
	// --user-data-dir argument names exactly this folder.
	if info.ExecutablePath == "" {
		return abort(fmt.Errorf("无法确认浏览器进程的可执行文件，已拒绝认定启动成功"))
	}
	if !strings.EqualFold(filepath.Clean(info.ExecutablePath), filepath.Clean(exe)) {
		return abort(fmt.Errorf("启动的进程与所选浏览器内核不一致"))
	}
	if !info.HasCommandLine {
		return abort(fmt.Errorf("无法读取浏览器进程的命令行，不能确认其使用的实例文件，已拒绝认定启动成功"))
	}
	if !commandLineUsesDir(info.CommandLine, launchDir) {
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
