package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/onbeee/DeBrowser/internal/capture"
	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/uia"
	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// Master-side observation for Stage 10 v2 sync. Semantic
// channels only, no input injection anywhere:
//
//   - CDP target/page events: tab create/close (identified by
//     target id, never by list position), committed navigations
//     (this is also how address-bar navigation syncs — by its
//     committed effect, never by reading typed text), reload
//     detection.
//   - Navigation-history polling: back/forward, applied to each
//     follower's OWN history stack.
//   - UI Automation polling: the master's tab strip (titles +
//     selection) and the menu button's expanded state. Tab
//     ACTIVATION is only followed when the strip order can be
//     bound to CDP target ids reliably (see bindTabOrder);
//     otherwise the operation is reported as not syncable
//     instead of activating a guessed tab.
//   - Window polling: master window geometry/show state,
//     applied to followers with no-activation window calls.
//   - Low-level hooks OBSERVE page-content input only and hand
//     it to the per-follower queues as CDP input. Input over
//     browser chrome is never mirrored.

// tabBoundInput reports whether an event only makes sense
// against the CURRENT tab: page mouse/keyboard/wheel input and
// navigation of the current page. While the tab mapping is
// unverified, these are withheld entirely — dispatching them
// could drive a follower's wrong (stale) tab.
func tabBoundInput(kind syncEventKind) bool {
	switch kind {
	case evMouseMove, evMouseButton, evMouseWheel, evKey,
		evNavigate, evReload, evHistoryDelta:
		return true
	}
	return false
}

func (s *syncSession) broadcast(ev syncEvent) {
	if tabBoundInput(ev.kind) && !s.tabMapOK.Load() {
		return
	}
	for _, id := range s.order {
		if w := s.workers[id]; w != nil {
			w.Enqueue(ev)
		}
	}
}

// setTabMapOK records a tab-mapping verification transition.
// Losing the mapping immediately stops tab-bound dispatch
// (broadcast gate), drops already-queued tab-bound input from
// every follower, and releases any input state held down —
// followers are never driven through a stale target while the
// mapping is unknown. Regaining it resumes dispatch; the tab
// poller re-attaches and re-activates right after. The session
// itself is not torn down for a mapping loss.
func (s *syncSession) setTabMapOK(ok bool) {
	if s.tabMapOK.Swap(ok) == ok {
		return // no transition
	}
	if ok {
		return
	}
	for _, w := range s.workers {
		// Gate first (inside suspendInput), then clear and
		// release: no concurrent enqueue can land afterwards.
		w.suspendInput()
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
		Title    string `json:"title"`
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
		s.targetURLs[p.TargetInfo.TargetID] = p.TargetInfo.URL
		s.targetTitles[p.TargetInfo.TargetID] = p.TargetInfo.Title
		s.mMu.Unlock()
		// Followers create their counterpart tab and bind it to
		// this target id; the committed URL follows through the
		// navigation channel, and activation through the
		// verified tab channel. The new tab's POSITION in the
		// strip is never assumed.
		s.broadcast(syncEvent{kind: evCreateTab, masterTarget: p.TargetInfo.TargetID})
	case "Target.targetDestroyed":
		var p targetEventParams
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		s.mMu.Lock()
		_, known := s.targetURLs[p.TargetID]
		delete(s.sessions, p.TargetID)
		delete(s.targetURLs, p.TargetID)
		delete(s.targetTitles, p.TargetID)
		wasCurrent := s.currentTarget == p.TargetID
		if wasCurrent {
			// The browser activates a neighbor; the tab poller
			// picks the new selection up once the mapping
			// verifies again.
			s.currentTarget = ""
		}
		s.mMu.Unlock()
		if wasCurrent {
			// The current target is gone: close the input
			// dispatch gate NOW — not at the next poll cycle.
			// Followers stop receiving input for the dead
			// target, their queued input is dropped and held
			// state released. The tab poller re-establishes
			// bindings and attachments once the browser's new
			// selection verifies.
			s.setTabMapOK(false)
		}
		if known {
			s.broadcast(syncEvent{kind: evCloseTab, masterTarget: p.TargetID})
		}
	case "Target.targetInfoChanged":
		var p targetEventParams
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		s.mMu.Lock()
		s.targetTitles[p.TargetInfo.TargetID] = p.TargetInfo.Title
		s.mMu.Unlock()
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

// bindTabOrder binds a UIA tab strip (titles in strip order) to
// CDP page targets. The strip exposes titles as the only tab
// identity, so the binding is accepted ONLY when the counts
// match and every title is unique on both sides — a duplicated
// title makes the correspondence ambiguous, and an ambiguous
// mapping must never drive an activation. Returns the target
// ids in strip order.
func bindTabOrder(titles []string, infos []cdp.TargetInfo) ([]string, bool) {
	pages := make([]cdp.TargetInfo, 0, len(infos))
	for _, ti := range infos {
		if ti.Type == "page" {
			pages = append(pages, ti)
		}
	}
	if len(titles) == 0 || len(pages) != len(titles) {
		return nil, false
	}
	byTitle := make(map[string]string, len(pages))
	for _, p := range pages {
		if _, dup := byTitle[p.Title]; dup {
			return nil, false
		}
		byTitle[p.Title] = p.TargetID
	}
	order := make([]string, len(titles))
	used := make(map[string]bool, len(titles))
	for i, title := range titles {
		id, ok := byTitle[title]
		if !ok || used[id] {
			return nil, false
		}
		used[id] = true
		order[i] = id
	}
	return order, true
}

// initialBindings computes the verified master -> follower
// target bindings at session start. When both strips bind
// reliably AND show the same title sequence, tabs correspond
// position-by-position (title-verified). Otherwise only the two
// currently selected tabs are bound, and only when both sides
// determine theirs reliably. Anything else stays unbound —
// activations for unbound tabs are reported, never guessed.
func initialBindings(masterTitles []string, masterInfos []cdp.TargetInfo, masterSel int,
	fTitles []string, fInfos []cdp.TargetInfo, fSel int) map[string]string {
	out := make(map[string]string)
	mOrder, mOK := bindTabOrder(masterTitles, masterInfos)
	fOrder, fOK := bindTabOrder(fTitles, fInfos)
	if !mOK || !fOK {
		return out
	}
	sameSeq := len(masterTitles) == len(fTitles)
	if sameSeq {
		for i := range masterTitles {
			if masterTitles[i] != fTitles[i] {
				sameSeq = false
				break
			}
		}
	}
	if sameSeq {
		for i := range mOrder {
			out[mOrder[i]] = fOrder[i]
		}
		return out
	}
	if masterSel >= 0 && masterSel < len(mOrder) && fSel >= 0 && fSel < len(fOrder) {
		out[mOrder[masterSel]] = fOrder[fSel]
	}
	return out
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

// tabPoller follows the master's selected tab. CDP does not
// report tab activation, and Target.getTargets order is NOT
// tab-strip order, so every cycle re-binds the UIA strip to
// target ids with bindTabOrder and only acts when the binding
// verifies. When it does not, tabMapOK is cleared (the page
// tells the user) and no activation is broadcast — a wrong tab
// is never activated on a guess. When the binding verifies, the
// master's current target is attached BEFORE the dispatch gate
// opens; on recovery every follower re-verifies and re-attaches
// through rebindFollowers before its own gate opens.
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
			s.setTabMapOK(false)
			continue
		}
		titles, sel, ok := uia.TabStrip(hwnd)
		if !ok || sel < 0 {
			s.setTabMapOK(false)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		infos, err := s.masterClient.Targets(cctx)
		cancel()
		if err != nil {
			s.setTabMapOK(false)
			continue
		}
		order, bound := bindTabOrder(titles, infos)
		if !bound || sel >= len(order) {
			s.setTabMapOK(false)
			continue
		}
		target := order[sel]
		s.advanceToTarget(titles, infos, sel, target)
	}
}

// advanceToTarget moves the session to a binding-verified
// master target. Whenever the target CHANGES (tab switch, or
// the previous current target was closed) or the mapping was
// down, the dispatch gate closes FIRST — every follower
// suspends (gate-first, queued input dropped, held state
// released) so no input can keep flowing to the OLD target —
// and only then is the master attached to the new target.
// After a successful master attach, followers re-verify their
// bindings from this cycle's fresh data and re-attach
// individually; the session gate being open never makes a
// follower ready on its own. On master attach failure the
// gate stays closed and the next cycle retries.
func (s *syncSession) advanceToTarget(titles []string, infos []cdp.TargetInfo, sel int, target string) {
	wasOK := s.tabMapOK.Load()
	changed := target != s.currentTargetSnapshot()
	if changed || !wasOK {
		s.setTabMapOK(false)
	}
	if !s.verifyAndAttachMaster(target) {
		return
	}
	if changed || !wasOK {
		s.rebindFollowers(titles, infos, sel, target)
	} else if s.followersUnready() && s.rebindTick%5 == 0 {
		s.rebindFollowers(titles, infos, sel, target)
	}
	s.rebindTick++
}

// verifyAndAttachMaster attaches the master session to the
// given (binding-verified) current target and only then opens
// the input dispatch gate. On attach failure the gate stays
// closed, the failure is recorded for the sync state, and
// false is returned; the caller retries on the next cycle.
func (s *syncSession) verifyAndAttachMaster(target string) bool {
	if target == s.currentTargetSnapshot() && s.masterAttached() {
		s.setMasterErr("")
		s.setTabMapOK(true)
		return true
	}
	if err := s.attachMasterTarget(target); err != nil {
		s.setMasterErr("附加主实例当前标签失败：" + err.Error())
		s.setTabMapOK(false)
		return false
	}
	s.setMasterErr("")
	s.refreshMasterViewport()
	s.setTabMapOK(true)
	return true
}

func (s *syncSession) masterAttached() bool {
	s.mMu.Lock()
	defer s.mMu.Unlock()
	return s.currentTarget != "" && s.sessions[s.currentTarget] != ""
}

func (s *syncSession) setMasterErr(msg string) {
	s.mMu.Lock()
	s.masterErr = msg
	s.mMu.Unlock()
}

func (s *syncSession) masterErrSnapshot() string {
	s.mMu.Lock()
	defer s.mMu.Unlock()
	return s.masterErr
}

func (s *syncSession) followersUnready() bool {
	for _, id := range s.order {
		w := s.workers[id]
		if w == nil {
			continue
		}
		if failed, _ := w.isFailed(); failed {
			continue
		}
		if ready, _ := w.isReady(); !ready {
			return true
		}
	}
	return false
}

// rebindFollowers recomputes every not-yet-ready follower's
// tab bindings from FRESH follower data (its current targets
// and tab strip) against the master's verified strip, and
// queues a rebind: the worker replaces its bindings and
// re-attaches to the master's current target, opening its own
// gate only on success. Followers that are ready or failed are
// left alone — one follower's recovery never disturbs another.
func (s *syncSession) rebindFollowers(masterTitles []string, masterInfos []cdp.TargetInfo, masterSel int, target string) {
	for _, fid := range s.order {
		w := s.workers[fid]
		if w == nil {
			continue
		}
		if failed, _ := w.isFailed(); failed {
			continue
		}
		if ready, _ := w.isReady(); ready {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		finfos, err := w.client.Targets(ctx)
		cancel()
		if err != nil {
			continue
		}
		var fcaps uia.Capabilities
		if pid := s.svc.instances.instanceProcessID(fid); pid > 0 {
			if hwnd := topHWND(uint32(pid)); hwnd != 0 {
				fcaps, _ = uia.ProbeWindow(hwnd)
			}
		}
		bindings := initialBindings(masterTitles, masterInfos, masterSel, fcaps.TabTitles, finfos, fcaps.SelectedTab)
		w.Rebind(bindings, target)
	}
}

// uiaPoller is the master-side UI Automation capture entry for
// browser-chrome state that polling can observe reliably. Its
// first detector follows the main menu button's expanded
// state: opening or closing the master's menu produces a
// semantic command that each follower executes on its own menu
// button (or reports as unexecutable). Menu ITEM selection is
// not observable this way and is not claimed — the effects of
// menu commands (tabs, navigations, windows) sync through
// their own channels.
func (s *syncSession) uiaPoller(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	menuInit := false
	var menuWas bool
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
		expanded, known := uia.MenuButtonState(hwnd)
		if !known {
			menuInit = false
			continue
		}
		if !menuInit {
			menuInit, menuWas = true, expanded
			continue
		}
		if expanded == menuWas {
			continue
		}
		menuWas = expanded
		if s.paused.Load() {
			continue
		}
		if expanded {
			s.broadcast(syncEvent{kind: evUIOp, uiOp: &uiOpCommand{
				Action: "expand", Spec: uia.MenuButtonSpec(), Label: "打开浏览器菜单",
			}})
		} else {
			s.broadcast(syncEvent{kind: evUIOp, uiOp: &uiOpCommand{
				Action: "collapse", Spec: uia.MenuButtonSpec(), Label: "关闭浏览器菜单",
			}})
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

// errCaptureEnded reports a capture start that arrived after
// the session's teardown had already run.
var errCaptureEnded = fmt.Errorf("同步会话已结束，无法开始捕获")

// startCapture starts this session's input capture exactly
// once. If teardown already ran (the master disconnected
// during initialization), the start is refused — hooks are
// never installed for a dead session.
func (s *syncSession) startCapture() error {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capEnded {
		return errCaptureEnded
	}
	if s.capOn {
		return nil
	}
	if err := s.svc.captureStart(func(ev capture.Event) { s.onCaptured(ev) }); err != nil {
		return err
	}
	s.capOn = true
	return nil
}

// stopCapture ends this session's capture exactly once; the
// underlying stop waits for the hook thread to exit, so no
// capture thread or handler outlives the session.
func (s *syncSession) stopCapture() {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capEnded {
		return
	}
	s.capEnded = true
	if s.capOn {
		s.svc.captureStop()
		s.capOn = false
	}
}

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
		// Browser chrome: not mirrored as input. Its effects
		// sync through the semantic channels (tabs, navigation,
		// windows, menu state). A click here is NOT counted as
		// an unsynced operation — only operations that were
		// actually attempted and failed are (see noteOpFailed).
		if ev.Kind == capture.EventMouseDown {
			s.focusCtx.Store(1)
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
