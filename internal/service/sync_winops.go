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
	wins, err := winmgr.ProcessWindows(info.PID)
	if err != nil {
		return 0, info, err
	}
	for _, hwnd := range wins {
		if winmgr.WindowOwnerPID(hwnd) == info.PID {
			return hwnd, info, nil
		}
	}
	return 0, info, fmt.Errorf("未找到实例的浏览器窗口")
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
		hwnd, _, err := s.resolveWindow(id)
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
			s.savedPlacements[hwnd] = pl
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
	snapshot := make(map[uintptr]winmgr.Placement, len(s.savedPlacements))
	for hwnd, pl := range s.savedPlacements {
		snapshot[hwnd] = pl
	}
	s.savedMu.Unlock()
	if len(snapshot) == 0 {
		return fmt.Errorf("没有可恢复的窗口排列")
	}
	var firstErr error
	for hwnd, pl := range snapshot {
		if err := s.setWindowPlacement(hwnd, pl); err != nil {
			if firstErr == nil {
				firstErr = err
			}
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
	for id := range sess.followers {
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
