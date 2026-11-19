package service

import (
	"fmt"
	"time"

	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// Window management for synchronization: resolving an
// instance's browser window, arranging windows on a monitor,
// restoring them, and mirroring the master window during a
// session. Every operation re-verifies the instance identity
// through the runtime and the window's owner PID at the point
// of use. Geometry never activates a window; state changes go
// through placements (documented foreground limitation).

// SyncMonitor is one display for the UI.
type SyncMonitor struct {
	ID      int    `json:"id"`
	Primary bool   `json:"primary"`
	Label   string `json:"label"`
}

// ListSyncMonitors enumerates the connected displays.
func (s *SyncService) ListSyncMonitors(operationID string) ([]SyncMonitor, error) {
	op := beginOp(s.Auditor, operationID, "sync", "monitors", "", "")
	monitors, err := winmgr.ListMonitors()
	if err != nil {
		finishOp(op, err)
		return nil, err
	}
	out := make([]SyncMonitor, 0, len(monitors))
	for _, m := range monitors {
		label := fmt.Sprintf("显示器 %d", m.ID+1)
		if m.Primary {
			label += "（主显示器）"
		}
		label += fmt.Sprintf(" · %d×%d", m.WorkArea.Width, m.WorkArea.Height)
		out = append(out, SyncMonitor{ID: m.ID, Primary: m.Primary, Label: label})
	}
	finishOp(op, nil)
	return out, nil
}

// resolveWindow finds the instance's top-level browser window
// after verifying the runtime identity and the window's owner.
func (s *SyncService) resolveWindow(instanceID string) (uintptr, SyncRuntimeInfo, error) {
	info, ok := s.instances.SyncRuntimeInfo(instanceID)
	if !ok || !info.Running || !info.Verified {
		return 0, info, fmt.Errorf("实例未运行或运行状态无法确认")
	}
	wins, err := winmgr.ListProcessWindows(info.PID)
	if err != nil {
		return 0, info, err
	}
	// The main window must be UNIQUELY identifiable; the
	// first window in enumeration order is never guessed.
	hwnd, err := winmgr.PickMainWindow(wins)
	if err != nil {
		return 0, info, err
	}
	if winmgr.WindowOwnerPID(hwnd) != info.PID {
		return 0, info, fmt.Errorf("窗口身份复核失败")
	}
	return hwnd, info, nil
}

// defaultVerifySavedWindow performs the restore-time identity
// re-check: the instance must still be running with the SAME
// pid and process creation time as when the placement was
// saved. (The caller additionally re-checks the HWND's
// current owner, catching handle recycling.)
func (s *SyncService) defaultVerifySavedWindow(hwnd uintptr, rec savedWindow) bool {
	if s.instances == nil || rec.InstanceID == "" || rec.PID <= 0 {
		return false
	}
	info, ok := s.instances.SyncRuntimeInfo(rec.InstanceID)
	if !ok || !info.Running || !info.Verified {
		return false
	}
	if info.PID != rec.PID || info.ProcessCreateAt != rec.CreatedAt {
		return false
	}
	// The handle itself must still belong to that process —
	// this is what catches HWND recycling.
	return winmgr.WindowOwnerPID(hwnd) == rec.PID
}

// ArrangeSyncWindows tiles the given instances' windows on
// one monitor's work area (screen coordinates). Each window's
// original placement is saved first; a window already saved
// keeps its original record.
func (s *SyncService) ArrangeSyncWindows(operationID string, instanceIDs []string, monitorID int) error {
	op := beginOp(s.Auditor, operationID, "sync", "arrange", "", "")
	err := s.arrange(instanceIDs, monitorID)
	finishOp(op, err)
	return err
}

func (s *SyncService) arrange(instanceIDs []string, monitorID int) error {
	if len(instanceIDs) == 0 {
		return fmt.Errorf("请选择要排列的实例")
	}
	monitors, err := winmgr.ListMonitors()
	if err != nil {
		return err
	}
	var work *winmgr.Rect
	for _, m := range monitors {
		if m.ID == monitorID {
			r := m.WorkArea
			work = &r
			break
		}
	}
	if work == nil {
		return fmt.Errorf("所选显示器不存在")
	}
	type winRef struct {
		hwnd    uintptr
		current winmgr.Placement
		screen  winmgr.Rect
	}
	wins := make([]winRef, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		hwnd, info, err := s.resolveWindow(id)
		if err != nil {
			return fmt.Errorf("实例 %s：%w", id, err)
		}
		pl, err := winmgr.GetPlacement(hwnd)
		if err != nil {
			return err
		}
		scr, err := winmgr.WindowScreenRect(hwnd)
		if err != nil {
			return err
		}
		s.savedMu.Lock()
		if _, kept := s.savedPlacements[hwnd]; !kept {
			s.savedPlacements[hwnd] = savedWindow{
				InstanceID: id, PID: info.PID,
				CreatedAt: info.ProcessCreateAt, Placement: pl,
			}
		}
		s.savedMu.Unlock()
		wins = append(wins, winRef{hwnd: hwnd, current: pl, screen: scr})
	}
	rects := winmgr.ArrangeRects(*work, len(wins))
	for i, w := range wins {
		restore, screen := planArrange(w.current, w.screen, rects[i])
		if restore != nil {
			if err := s.setWindowPlacement(w.hwnd, *restore); err != nil {
				return err
			}
		}
		if screen != nil {
			if err := winmgr.SetWindowRect(w.hwnd, *screen); err != nil {
				return err
			}
		}
	}
	return nil
}

// RestoreSyncWindows restores every arranged window to its
// saved placement. Records are consumed per window: a window
// that restores successfully drops its record, a failure keeps
// it for a later retry and does not block the others.
func (s *SyncService) RestoreSyncWindows(operationID string) error {
	op := beginOp(s.Auditor, operationID, "sync", "restore", "", "")
	err := s.restore()
	finishOp(op, err)
	return err
}

func (s *SyncService) restore() error {
	s.savedMu.Lock()
	snapshot := make(map[uintptr]savedWindow, len(s.savedPlacements))
	for hwnd, rec := range s.savedPlacements {
		snapshot[hwnd] = rec
	}
	s.savedMu.Unlock()
	if len(snapshot) == 0 {
		return fmt.Errorf("没有可恢复的窗口排列")
	}
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for hwnd, rec := range snapshot {
		// Identity first: the instance must still be the
		// same process AND the handle must still belong to
		// it. A record whose identity died can never succeed
		// on retry, so it is dropped and reported — never
		// silently treated as restored, and the (possibly
		// recycled) window is never touched.
		verify := s.verifySavedWindow
		if verify == nil {
			verify = s.defaultVerifySavedWindow
		}
		if !verify(hwnd, rec) {
			s.savedMu.Lock()
			delete(s.savedPlacements, hwnd)
			s.savedMu.Unlock()
			note(fmt.Errorf("一个窗口的身份已失效，已跳过并清除其恢复记录"))
			continue
		}
		if err := s.setWindowPlacement(hwnd, rec.Placement); err != nil {
			note(err)
			continue
		}
		s.savedMu.Lock()
		delete(s.savedPlacements, hwnd)
		s.savedMu.Unlock()
	}
	return firstErr
}

// planArrange is the service-level indirection over the
// winmgr planning rule (kept package-visible for tests).
func planArrange(current winmgr.Placement, currentScreen, target winmgr.Rect) (*winmgr.Placement, *winmgr.Rect) {
	return winmgr.PlanArrange(current, currentScreen, target)
}

// startMirror begins the master-window mirroring loop for a
// session: every 300 ms the master's placement and actual
// screen rectangle are compared with the last mirrored state;
// changes are applied to followers through the winmgr rules
// (geometry via the non-activating path, state via placement;
// a minimized master is not mirrored). Windows whose identity
// cannot be re-verified are skipped.
func (s *SyncService) startMirror(sess *syncSession) {
	masterHWND, _, err := s.resolveWindow(sess.masterID)
	if err != nil || masterHWND == 0 {
		return
	}
	sess.masterHWND = masterHWND
	for id := range sess.followersSnapshot() {
		if hwnd, _, err := s.resolveWindow(id); err == nil && hwnd != 0 {
			sess.followerHWNDs[id] = hwnd
		}
	}
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-sess.ctx.Done():
				return
			case <-tick.C:
				s.mirrorOnce(sess)
			}
		}
	}()
}

func (s *SyncService) mirrorOnce(sess *syncSession) {
	if !s.instances.VerifySyncIdentity(sess.master) {
		return
	}
	if winmgr.WindowOwnerPID(sess.masterHWND) != sess.master.PID {
		return
	}
	pl, err := winmgr.GetPlacement(sess.masterHWND)
	if err != nil {
		return
	}
	scr, err := winmgr.WindowScreenRect(sess.masterHWND)
	if err != nil {
		return
	}
	sess.mu.Lock()
	if sess.lastMasterPl != nil && winmgr.PlacementsEqual(*sess.lastMasterPl, pl) &&
		sess.lastMasterScr != nil && *sess.lastMasterScr == scr {
		sess.mu.Unlock()
		return
	}
	sess.lastMasterPl = &pl
	sess.lastMasterScr = &scr
	sess.mu.Unlock()

	for id, hwnd := range sess.followerHWNDs {
		w := sess.followerByID(id)
		if w == nil || w.isFailed() {
			continue
		}
		info, ok := s.instances.SyncRuntimeInfo(id)
		if !ok || !s.instances.VerifySyncIdentity(info) {
			continue
		}
		if winmgr.WindowOwnerPID(hwnd) != info.PID {
			continue
		}
		fpl, err := winmgr.GetPlacement(hwnd)
		if err != nil {
			continue
		}
		screen, placement := winmgr.PlanWindowMirror(pl, scr, fpl)
		if screen != nil {
			_ = winmgr.SetWindowRect(hwnd, *screen)
		} else if placement != nil {
			_ = s.setWindowPlacement(hwnd, *placement)
		}
	}
}
