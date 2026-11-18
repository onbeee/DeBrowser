package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/onbeee/DeBrowser/internal/capture"
	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/uia"
	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// Master-side observation for Stage 10 v2 sync. Three semantic
// channels, no input injection anywhere:
//
//   - CDP target/page events: tab create/close, committed
//     navigations (this is also how address-bar navigation
//     syncs — by its committed effect, never by reading typed
//     text), reload detection.
//   - Navigation-history polling: back/forward, applied to each
//     follower's OWN history stack.
//   - UI Automation polling: which tab is selected in the
//     master's tab strip (CDP does not report tab activation).
//   - Window polling: master window geometry/show state,
//     applied to followers with no-activation window calls.
//   - Low-level hooks OBSERVE page-content input only and hand
//     it to the per-follower queues as CDP input. Input over
//     browser chrome is never mirrored; the semantic channels
//     above cover what they reliably can, and the rest is
//     counted and reported as not semantically syncable.

func (s *syncSession) broadcast(ev syncEvent) {
	for _, id := range s.order {
		if w := s.workers[id]; w != nil {
			w.Enqueue(ev)
		}
	}
}

func (s *syncSession) attachMasterTarget(targetID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sid, err := s.masterClient.Attach(ctx, targetID)
	if err != nil {
		return err
	}
	s.mMu.Lock()
	s.sessions[targetID] = sid
	s.currentTarget = targetID
	if u := s.targetURLs[targetID]; u != "" {
		s.currentURL = u
	}
	s.histInit = false
	s.mMu.Unlock()
	return nil
}

func (s *syncSession) refreshMasterViewport() {
	s.mMu.Lock()
	sid := s.sessions[s.currentTarget]
	s.mMu.Unlock()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if w, h, err := s.masterClient.LayoutViewport(ctx, sid); err == nil && w > 0 {
		s.mMu.Lock()
		s.masterViewport.w, s.masterViewport.h = w, h
		s.mMu.Unlock()
		for _, worker := range s.workers {
			worker.RefreshViewport(w, h)
		}
	}
}

type targetEventParams struct {
	TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
		URL      string `json:"url"`
	} `json:"targetInfo"`
	TargetID string `json:"targetId"`
}

type frameNavigatedParams struct {
	Frame struct {
		ID       string `json:"id"`
		ParentID string `json:"parentId"`
		URL      string `json:"url"`
		LoaderID string `json:"loaderId"`
	} `json:"frame"`
}

// onMasterEvent handles the master's CDP events. It runs on the
// CDP client's delivery goroutine and may issue commands on the
// master client (the client decouples delivery from reading).
func (s *syncSession) onMasterEvent(ev cdp.Event) {
	switch ev.Method {
	case "Target.targetCreated":
		var p targetEventParams
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		s.mMu.Lock()
		s.targets = append(s.targets, p.TargetInfo.TargetID)
		s.targetURLs[p.TargetInfo.TargetID] = p.TargetInfo.URL
		index := len(s.targets) - 1
		s.mMu.Unlock()
		// Followers open a blank tab at the same position; the
		// committed URL follows through the navigation channel.
		s.broadcast(syncEvent{kind: evCreateTab, index: index, url: "about:blank"})
	case "Target.targetDestroyed":
		var p targetEventParams
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		s.mMu.Lock()
		index := -1
		for i, t := range s.targets {
			if t == p.TargetID {
				index = i
				break
			}
		}
		if index < 0 {
			s.mMu.Unlock()
			return
		}
		s.targets = append(s.targets[:index], s.targets[index+1:]...)
		delete(s.sessions, p.TargetID)
		delete(s.targetURLs, p.TargetID)
		wasCurrent := s.currentTarget == p.TargetID
		s.mMu.Unlock()
		s.broadcast(syncEvent{kind: evCloseTab, index: index})
		if wasCurrent {
			// The browser activates a neighbor; the tab poller
			// picks the new selection up within one interval.
			s.mMu.Lock()
			s.currentTarget = ""
			s.mMu.Unlock()
		}
	case "Target.targetInfoChanged":
		var p targetEventParams
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		s.noteTargetURL(p.TargetInfo.TargetID, p.TargetInfo.URL)
	case "Page.frameNavigated":
		if ev.SessionID == "" {
			return
		}
		s.mMu.Lock()
		isCurrent := s.sessions[s.currentTarget] == ev.SessionID
		s.mMu.Unlock()
		if !isCurrent {
			return
		}
		var p frameNavigatedParams
		if json.Unmarshal(ev.Params, &p) != nil || p.Frame.ParentID != "" {
			return
		}
		s.mMu.Lock()
		sameURL := p.Frame.URL == s.currentURL && s.currentURL != ""
		reload := sameURL && p.Frame.LoaderID != "" && s.lastLoaderID != "" && p.Frame.LoaderID != s.lastLoaderID
		if p.Frame.LoaderID != "" {
			s.lastLoaderID = p.Frame.LoaderID
		}
		s.mMu.Unlock()
		if reload {
			s.broadcast(syncEvent{kind: evReload})
		} else if !sameURL {
			s.noteTargetURL(s.currentTargetSnapshot(), p.Frame.URL)
		}
	}
}

func (s *syncSession) currentTargetSnapshot() string {
	s.mMu.Lock()
	defer s.mMu.Unlock()
	return s.currentTarget
}

// noteTargetURL records a target's committed URL; when it is the
// current target's, the navigation is broadcast once per change.
func (s *syncSession) noteTargetURL(targetID, url string) {
	if url == "" || url == "about:blank" {
		return
	}
	s.mMu.Lock()
	changed := s.targetURLs[targetID] != url
	s.targetURLs[targetID] = url
	isCurrent := s.currentTarget == targetID
	urlChanged := s.currentURL != url
	if isCurrent {
		s.currentURL = url
	}
	s.mMu.Unlock()
	if changed && isCurrent && urlChanged {
		s.broadcast(syncEvent{kind: evNavigate, url: url})
	}
}

// historyPoller detects back/forward on the master by watching
// its current page's navigation-history index. Entry changes
// (new navigations) are handled by the navigation channel; only
// a pure index move on an unchanged stack is a back/forward.
func (s *syncSession) historyPoller(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.mMu.Lock()
		sid := s.sessions[s.currentTarget]
		s.mMu.Unlock()
		if sid == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		entries, current, err := s.masterClient.NavigationHistory(cctx, sid)
		cancel()
		if err != nil || len(entries) == 0 || current < 0 || current >= len(entries) {
			continue
		}
		s.mMu.Lock()
		if !s.histInit {
			s.histLen, s.histIndex, s.histEntryURL, s.histInit = len(entries), current, entries[current].URL, true
			s.mMu.Unlock()
			continue
		}
		sameStack := len(entries) == s.histLen && entries[current].URL == s.histEntryURL
		delta := current - s.histIndex
		if sameStack && delta != 0 {
			s.histIndex = current
			s.mMu.Unlock()
			if !s.paused.Load() {
				s.broadcast(syncEvent{kind: evHistoryDelta, delta: delta})
			}
			continue
		}
		s.histLen, s.histIndex, s.histEntryURL = len(entries), current, entries[current].URL
		s.mMu.Unlock()
	}
}

// tabPoller follows the master's selected tab through UI
// Automation — CDP does not report which tab is active — and
// broadcasts activations. When UIA cannot read the tab strip,
// tab switching by clicking is the one tab operation that
// cannot be followed; creation and closure still are (CDP).
func (s *syncSession) tabPoller(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		hwnd := topHWND(s.masterPID)
		if hwnd == 0 {
			continue
		}
		idx, ok := uia.SelectedTabIndex(hwnd)
		if !ok || idx < 0 {
			continue
		}
		s.mMu.Lock()
		if idx == s.lastMasterTab || idx >= len(s.targets) {
			s.mMu.Unlock()
			continue
		}
		s.lastMasterTab = idx
		target := s.targets[idx]
		s.mMu.Unlock()
		if err := s.attachMasterTarget(target); err != nil {
			continue
		}
		s.refreshMasterViewport()
		if !s.paused.Load() {
			s.broadcast(syncEvent{kind: evActivateTab, index: idx})
		}
	}
}

// windowPoller mirrors the master window's geometry and show
// state onto followers using no-activation window calls, and
// refreshes viewports when the master's size changes. Follower
// windows are never read back, so no feedback loop can form.
func (s *syncSession) windowPoller(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		win, ok := winmgr.TopWindowForPID(s.masterPID)
		if !ok {
			continue
		}
		s.mMu.Lock()
		if !s.winInit {
			s.winInit = true
			s.lastRect, s.lastState = win.Rect, win.State
			s.mMu.Unlock()
			continue
		}
		rectChanged := win.Rect != s.lastRect
		stateChanged := win.State != s.lastState
		s.lastRect, s.lastState = win.Rect, win.State
		s.mMu.Unlock()
		if !rectChanged && !stateChanged {
			continue
		}
		for _, fid := range s.order {
			pid := uint32(s.svc.instances.instanceProcessID(fid))
			fwin, ok := winmgr.TopWindowForPID(pid)
			if !ok {
				continue
			}
			if stateChanged && win.State != winmgr.StateNormal {
				_ = winmgr.SetWindowState(fwin.HWND, win.State)
				continue
			}
			if stateChanged && win.State == winmgr.StateNormal && fwin.State != winmgr.StateNormal {
				_ = winmgr.SetWindowState(fwin.HWND, winmgr.StateNormal)
			}
			if rectChanged {
				_ = winmgr.SetWindowRect(fwin.HWND, win.Rect)
			}
		}
		if rectChanged {
			s.refreshMasterViewport()
		}
	}
}

// ---- input capture ingress ------------------------------------------

func (s *syncSession) startCapture() error {
	return capture.StartCapture(func(ev capture.Event) { s.onCaptured(ev) })
}

func (s *syncSession) stopCapture() { capture.StopCapture() }

// onCaptured runs on the hook thread: it must never block. It
// classifies the event and offers copies to the follower queues.
func (s *syncSession) onCaptured(ev capture.Event) {
	if s.paused.Load() {
		return
	}
	fg := winmgr.ForegroundWindow()
	if fg == 0 || winmgr.WindowPID(fg) != s.masterPID {
		return
	}
	switch ev.Kind {
	case capture.EventKeyDown, capture.EventKeyUp:
		s.onCapturedKey(ev)
		return
	}
	hit := winmgr.ClassifyPoint(ev.X, ev.Y, s.masterPID)
	if !hit.InProcess {
		return
	}
	if !hit.IsContent {
		// Browser chrome: not mirrored. Semantic channels cover
		// tabs/navigation/windows; anything else is counted so
		// the page can report it honestly.
		if ev.Kind == capture.EventMouseDown {
			s.focusCtx.Store(1)
			s.uiSkipped.Add(1)
		}
		return
	}
	if ev.Kind == capture.EventMouseDown {
		s.focusCtx.Store(0)
	}
	s.mMu.Lock()
	vw, vh := s.masterViewport.w, s.masterViewport.h
	s.mMu.Unlock()
	if vw <= 0 || vh <= 0 || hit.ContentRect.Width <= 0 || hit.ContentRect.Height <= 0 {
		return
	}
	cssX := float64(ev.X-hit.ContentRect.X) * vw / float64(hit.ContentRect.Width)
	cssY := float64(ev.Y-hit.ContentRect.Y) * vh / float64(hit.ContentRect.Height)
	switch ev.Kind {
	case capture.EventMouseMove:
		s.broadcast(syncEvent{kind: evMouseMove, x: cssX, y: cssY, buttons: ev.ButtonsMask})
	case capture.EventMouseDown:
		s.broadcast(syncEvent{kind: evMouseButton, x: cssX, y: cssY, button: ev.Button, buttons: ev.ButtonsMask, down: true, clickCount: 1})
	case capture.EventMouseUp:
		s.broadcast(syncEvent{kind: evMouseButton, x: cssX, y: cssY, button: ev.Button, buttons: ev.ButtonsMask, down: false, clickCount: 1})
	case capture.EventMouseWheel:
		s.broadcast(syncEvent{kind: evMouseWheel, x: cssX, y: cssY, buttons: ev.ButtonsMask,
			deltaX: float64(ev.DeltaX), deltaY: float64(-ev.DeltaY)})
	}
}

func (s *syncSession) onCapturedKey(ev capture.Event) {
	key, code, text, isModifier := syncKeyForVK(ev.VK)
	if key == "" {
		return
	}
	down := ev.Kind == capture.EventKeyDown
	// Focus-context tracking protects the followers' pages from
	// keystrokes aimed at browser chrome (typing in the address
	// bar must never land in a follower's page). Shortcuts that
	// move focus to chrome mark the context; Escape returns it.
	if down && isChromeFocusShortcut(ev.VK) {
		s.focusCtx.Store(1)
		return
	}
	if s.focusCtx.Load() == 1 {
		if down && ev.VK == vkEscape {
			s.focusCtx.Store(0)
		}
		return
	}
	modifiers := currentModifiers()
	evOut := syncEvent{
		kind: evKey, key: key, code: code, vk: int(ev.VK), down: down, modifiers: modifiers,
	}
	if down && !isModifier {
		evOut.text = text
	}
	s.broadcast(evOut)
}

// vkEscape is the virtual-key code for Escape.
const vkEscape = 0x1B

// isChromeFocusShortcut reports keys that move keyboard focus
// into browser chrome: Ctrl+L / Alt+D (address bar), Ctrl+F
// (find bar), F6 (pane focus).
func isChromeFocusShortcut(vk uint32) bool {
	mods := currentModifiers()
	ctrl := mods&2 != 0
	alt := mods&1 != 0
	switch vk {
	case 0x4C: // L
		return ctrl
	case 0x44: // D
		return alt
	case 0x46: // F
		return ctrl
	case 0x75: // F6
		return true
	}
	return false
}
